package function

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/trace"
)

func init() {
	config = &configuration{}
}

var (
	config        *configuration
	weatherApiUrl string
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

type configuration struct {
	once sync.Once
	err  error
}

func (c *configuration) Error() error {
	return c.err
}

type envError struct {
	name string
}

func (e *envError) Error() string {
	return fmt.Sprintf("%s environment variable unset or missing", e.name)
}

func F(w http.ResponseWriter, r *http.Request) {
	config.once.Do(func() { configFunc() })
	if config.Error() != nil {
		log.Println(config.Error())
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

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

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var webhookRequest WebhookRequest
	err = json.Unmarshal(body, &webhookRequest)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	parameters := webhookRequest.QueryResult.Parameters

	weather, err := getWeather(ctx, parameters["event"])
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	response := &WebhookResponse{
		FulfillmentText: fmt.Sprintf("The current temperature in %s is %d degrees fahrenheit.",
			weather.Location, weather.Temperature),
	}

	data, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		log.Println(err)
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

func defaultConfigFunc() {
	weatherApiUrl = os.Getenv("WEATHER_API_URL")
	if weatherApiUrl == "" {
		config.err = &envError{"WEATHER_API_URL"}
		return
	}

	projectId := os.Getenv("GCP_PROJECT")
	if projectId == "" {
		config.err = &envError{"GCP_PROJECT"}
		return
	}

	stackdriverExporter, err := stackdriver.NewExporter(stackdriver.Options{ProjectID: projectId})
	if err != nil {
		config.err = err
		return
	}

	trace.RegisterExporter(stackdriverExporter)
	trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})
}
