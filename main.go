package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/windows/registry"
)

type Sensor struct {
	Sensor   string  `json:"sensor"`
	Label    string  `json:"label"`
	Value    string  `json:"value"`
	ValueRaw float64 `json:"value_raw"`
}

type SensorCache struct {
	sensors []Sensor
	mu      sync.RWMutex
}

var (
	cache       = &SensorCache{}
	sensorGauge = newSensorGauge()
	serverPort  = getEnvOrDefault("HWINFOSERVER_PORT", ":56765")
)

func init() {
	prometheus.MustRegister(sensorGauge)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go updateSensorsLoop(ctx)
	startHTTPServer(cancel)
	<-ctx.Done()
	time.Sleep(1 * time.Second)
}

func newSensorGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hwinfo_sensor_value",
			Help: "Value of HWiNFO sensor (ValueRaw)",
		},
		[]string{"sensor", "label"},
	)
}

func updateSensorsLoop(ctx context.Context) {
	prevKeys := make(map[string]struct{})
	minSleepTime := 500 * time.Millisecond
	sleepTime := minSleepTime
	maxSleepTime := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			log.Println("Sensor update loop stopped.")
			return
		default:
			sensors := readSensors()
			currKeys := make(map[string]struct{})
			for _, s := range sensors {
				key := s.Sensor + "\x00" + s.Label
				currKeys[key] = struct{}{}
			}

			needReset := false
			if len(prevKeys) > 0 {
				for key := range prevKeys {
					if _, ok := currKeys[key]; !ok {
						needReset = true
						break
					}
				}
			}

			cache.mu.Lock()
			cache.sensors = sensors
			cache.mu.Unlock()

			if needReset {
				sensorGauge.Reset()
			}
			for _, s := range sensors {
				sensorGauge.WithLabelValues(s.Sensor, s.Label).Set(s.ValueRaw)
			}

			prevKeys = currKeys

			// Exponential backoff if no sensors, else reset backoff
			if len(sensors) == 0 {
				time.Sleep(sleepTime)
				sleepTime *= 2
				if sleepTime > maxSleepTime {
					sleepTime = maxSleepTime
				}
			} else {
				sleepTime = minSleepTime
				time.Sleep(sleepTime)
			}
		}
	}
}

func startHTTPServer(cancel context.CancelFunc) {
	http.HandleFunc("/sensors", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		if err := json.NewEncoder(w).Encode(cache.sensors); err != nil {
			http.Error(w, "Failed to encode JSON", http.StatusInternalServerError)
		}
	})

	http.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{Addr: serverPort}
	go func() {
		log.Printf("Serving JSON data at http://localhost%s/sensors and Prometheus metrics at http://localhost%s/metrics", serverPort, serverPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	handleGracefulShutdown(srv, cancel)
}

func handleGracefulShutdown(srv *http.Server, cancel context.CancelFunc) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Interrupt signal received. Shutting down server...")
	cancel() // stop sensor update goroutine
	ctx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server gracefully stopped.")
}

func readSensors() []Sensor {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\HWiNFO64\VSB`, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			log.Printf("HWiNFO registry key not found, will retry with backoff...")
		} else {
			log.Printf("Error opening HWiNFO registry key: %v", err)
		}
		return []Sensor{}
	}
	defer k.Close()

	var sensors []Sensor
	for i := 0; ; i++ {
		s, _, err := k.GetStringValue(fmt.Sprintf("Sensor%d", i))
		if err != nil {
			if errors.Is(err, registry.ErrNotExist) {
				break
			}
			log.Printf("Failed to read Sensor%d key: %v", i, err)
			continue
		}

		label, _, err := k.GetStringValue(fmt.Sprintf("Label%d", i))
		if err != nil {
			log.Printf("Failed to read Label%d key: %v", i, err)
			continue
		}

		value, _, err := k.GetStringValue(fmt.Sprintf("Value%d", i))
		if err != nil {
			log.Printf("Failed to read Value%d key: %v", i, err)
			continue
		}

		valueRawStr, _, err := k.GetStringValue(fmt.Sprintf("ValueRaw%d", i))
		if err != nil {
			log.Printf("Failed to read ValueRaw%d key: %v", i, err)
			continue
		}
		valueRaw, err := strconv.ParseFloat(valueRawStr, 64)
		if err != nil {
			log.Printf("Failed to parse ValueRaw%d key: %v", i, err)
			continue
		}

		sensor := Sensor{
			Sensor:   s,
			Label:    label,
			Value:    value,
			ValueRaw: valueRaw,
		}
		sensors = append(sensors, sensor)
	}
	return sensors
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
