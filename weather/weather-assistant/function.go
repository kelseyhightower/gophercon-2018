package function

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"

	"cloud.google.com/go/logging"
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/trace"
)

var (
	logger        *logging.Logger
	once          sync.Once
	weatherApiUrl string
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

func F(w http.ResponseWriter, r *http.Request) {
	once.Do(func() {
		if err := configFunc(); err != nil {
			panic(err)
		}
	})

	defer logger.Flush()

	ctx := r.Context()
	var span *trace.Span

	httpFormat := &propagation.HTTPFormat{}
	sc, ok := httpFormat.SpanContextFromRequest(r)
	if ok {
		ctx, span = trace.StartSpanWithRemoteParent(ctx, "weather-assistant", sc,
			trace.WithSampler(trace.AlwaysSample()),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var webhookRequest WebhookRequest
	err = json.Unmarshal(data, &webhookRequest)
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	parameters := webhookRequest.QueryResult.Parameters

	weather, err := getWeather(ctx, parameters["event"])
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	response := &WebhookResponse{
		FulfillmentText: fmt.Sprintf("The current temperature in %s is %d degrees fahrenheit.",
			weather.Location, weather.Temperature),
	}

	data, err = json.MarshalIndent(response, "", " ")
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func getWeather(ctx context.Context, event string) (*Weather, error) {
	ctx, span := trace.StartSpan(ctx, "weather-api")
	defer span.End()

	u := fmt.Sprintf("%s?event=%s", weatherApiUrl, url.QueryEscape(event))

	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("non 200 response code: %s", string(body))
	}

	var w *Weather
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}

	return w, nil
}

func defaultConfigFunc() error {
	var err error

	weatherApiUrl = os.Getenv("WEATHER_API_URL")
	if weatherApiUrl == "" {
		return fmt.Errorf("WEATHER_API_URL environment variable unset or missing")
	}

	if err := EnableStackdriverTrace(); err != nil {
		return err
	}

	logger, err = NewStackdriverLogger()
	if err != nil {
		return err
	}

	return nil
}
