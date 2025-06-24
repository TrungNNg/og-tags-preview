package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/TrungNNg/og-tag/internal/ogtags"
	"github.com/TrungNNg/og-tag/internal/ogtags_cache"
	"github.com/TrungNNg/og-tag/pkg/metrics"
	"github.com/TrungNNg/og-tag/pkg/redisclient"
	"github.com/TrungNNg/og-tag/pkg/worker"
	"github.com/go-playground/validator/v10"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/joho/godotenv"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type config struct {
	port               string
	env                string
	serverIdleTimeout  int
	serverReadTimeout  int
	serverWriteTimeout int

	redisAddr string
	redisPass string
	redisDB   int
}

type application struct {
	cfg       *config
	server    *http.Server
	client    ogtags.OGTagClient
	cache     ogtags_cache.OGCacheClient
	validator *validator.Validate
}

func newApplication(cfg *config) *application {
	// init validator
	validator := validator.New()

	// init client to fetch og tag of given url
	client := ogtags.New(retryablehttp.NewClient())

	// init otel
	// opentel.SetupOTelSDK()
	// slog.Info("opentelemetry established :)")

	// init redis connection
	redisConfig := redisclient.RedisConfig{
		Address:  cfg.redisAddr,
		Password: cfg.redisPass,
		DB:       cfg.redisDB,
	}
	rc := redisclient.New(redisConfig)
	slog.Info("connected to Redis :)")

	// init redis cache for popular url
	ogtagCache := ogtags_cache.New(rc)

	app := &application{
		cfg:       cfg,
		client:    client,
		cache:     ogtagCache,
		validator: validator,
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%s", app.cfg.port),
		Handler:      app.routes(),
		IdleTimeout:  time.Duration(cfg.serverIdleTimeout) * time.Minute,
		ReadTimeout:  time.Duration(cfg.serverReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.serverWriteTimeout) * time.Second,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
	app.server = server
	return app
}

func (app *application) run() {
	shutdownError := make(chan error)
	go func() {
		defer func() {
			pv := recover()
			if pv != nil {
				slog.Error(fmt.Sprintf("%v", pv))
			}
		}()
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		s := <-quit

		slog.Info("shutting down server", "signal", s.String())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Shutdown() will return nil if the graceful shutdown was successful, else an error
		err := app.server.Shutdown(ctx)
		if err != nil {
			shutdownError <- err
		}

		// shutdown otel
		//slog.Info("wait for otel shutdown to complete")
		//if err := opentel.Shutdown(ctx); err != nil {
		//	slog.Error("otel shutdown failed", "error", err.Error())
		//} else {
		//	slog.Info("otel shutdown completed")
		//}

		// waiting for any background goroutines to complete their tasks.
		// send nil to shutdownError to signal shutdown complete
		slog.Info("wait for background tasks to complete")
		worker.Wait()
		slog.Info("all backgound task completed")
		shutdownError <- nil
	}()

	slog.Info("starting server", "addr", app.server.Addr, "env", app.cfg.env)
	err := app.server.ListenAndServe()
	// Calling Shutdown() on our server will cause ListenAndServe() to immediately
	// return a http.ErrServerClosed error. If the err is NOT ErrServerClosed that mean
	// graceful shutdown failed
	if !errors.Is(err, http.ErrServerClosed) {
		slog.Error(err.Error())
		os.Exit(1)
	}

	// If shutdownError got non-nil error it mean graceful shutdown failed
	err = <-shutdownError
	if err != nil {
		slog.Error("graceful shutdown failed", "error", err.Error())
		os.Exit(1)
	}

	slog.Info("stopped server", "address", app.server.Addr)
}

func (app *application) routes() http.Handler {
	router := httprouter.New()

	router.HandlerFunc(http.MethodGet, "/health", app.healthcheckHandler)
	router.HandlerFunc(http.MethodPost, "/og", app.ogTagHandler)

	// Prometheus metrics endpoint
	router.Handler(http.MethodGet, "/metrics", promhttp.Handler())

	return app.recoverPanic(router)
	//return otelhttp.NewHandler(router, "server")
}

func (app *application) healthcheckHandler(w http.ResponseWriter, r *http.Request) {
	//ctx, span := opentel.Tracer.Start(r.Context(), "health")
	//defer span.End()
	endpoint := "/health"

	metrics.Inc(endpoint)
	start := time.Now()

	metrics.CountResponse(http.StatusOK, endpoint)
	app.writeJSON(w, http.StatusOK, envelope{
		"status": "available",
	}, nil)

	metrics.Latency([]string{endpoint}, time.Since(start))
}

func (app *application) ogTagHandler(w http.ResponseWriter, r *http.Request) {
	endpoint := "/og"
	metrics.Inc(endpoint)

	var input struct {
		URL string `json:"url" validate:"required,url"`
	}

	err := app.readJSON(w, r, &input)
	if err != nil {
		metrics.CountResponse(http.StatusBadRequest, endpoint)
		app.badRequestResponse(w, r, err)
		return
	}

	var validationErrors validator.ValidationErrors
	err = app.validator.Struct(input)
	if err != nil {
		if errors.As(err, &validationErrors) {
			metrics.CountResponse(http.StatusUnprocessableEntity, endpoint)
			app.failedValidationResponse(w, r, validationErrors)
			return
		}
		metrics.CountResponse(http.StatusInternalServerError, endpoint)
		app.serverErrorResponse(w, r, err)
		return
	}

	// check cache
	cachedJSON, err := app.cache.Get(input.URL)
	if err != nil {
		if errors.Is(err, ogtags_cache.ErrKeyNotFound) {
			slog.Info("cache missed")
		} else {
			slog.Info("ogTagHandler:app.cache.Get", "error", err)
		}
	} else {
		metrics.CacheHit()
		slog.Info("cache hit")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cachedJSON))
		return
	}
	metrics.CacheMiss()

	// Fetch og tags from url
	ogs, err := app.client.GetOGTags(input.URL)
	if err != nil {
		metrics.CountResponse(http.StatusInternalServerError, endpoint)
		app.serverErrorResponse(w, r, err)
		return
	}

	response := envelope{"result": ogs}

	// return json, not cache result if writeJSON failed
	err = app.writeJSON(w, http.StatusOK, response, nil)
	if err != nil {
		metrics.CountResponse(http.StatusInternalServerError, endpoint)
		app.serverErrorResponse(w, r, err)
		return
	}

	// cached, make sure cached value similar to writeJSON result
	jsonBytes, err := json.MarshalIndent(response, "", "\t")
	if err != nil {
		slog.Error("ogTagHandler:MarshalIndent", "error", err)
		return
	}
	jsonBytes = append(jsonBytes, '\n')
	err = app.cache.Set(input.URL, jsonBytes)
	if err != nil {
		slog.Error("ogTagHandler:app.cache.Set", "error", err)
		return
	}
}

func loadConfig() *config {
	_ = godotenv.Load()

	getEnv := func(key string, required bool) string {
		val := os.Getenv(key)
		if val == "" && required {
			slog.Error("Missing required env var", slog.String("key", key))
			os.Exit(1)
		}
		return val
	}

	getInt := func(key string, required bool) int {
		valStr := getEnv(key, required)
		if valStr == "" {
			return 0
		}
		val, err := strconv.Atoi(valStr)
		if err != nil {
			slog.Error("Invalid int value", slog.String("key", key), slog.String("value", valStr))
			os.Exit(1)
		}
		return val
	}

	return &config{
		env:                getEnv("ENV", false),
		port:               getEnv("PORT", true),
		serverIdleTimeout:  getInt("SERVER_IDLETIMEOUT", true),
		serverReadTimeout:  getInt("SERVER_READTIMEOUT", true),
		serverWriteTimeout: getInt("SERVER_WRITETIMEOUT", true),
		redisAddr:          getEnv("REDIS_ADDR", true),
		redisPass:          getEnv("REDIS_PASSWORD", false),
		redisDB:            getInt("REDIS_DB", true),
	}
}
