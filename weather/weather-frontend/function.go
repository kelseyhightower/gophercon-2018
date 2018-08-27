package function

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"text/template"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/trace"
)

func init() {
	config = &configuration{}
}

var (
	config       *configuration
	httpClient   *http.Client
	htmlTemplate *template.Template
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

type Weather struct {
	Event       string
	Location    string
	Temperature int
}

type Events []Event

type Event struct {
	Name     string
	Selected bool
}

func F(w http.ResponseWriter, r *http.Request) {
	config.once.Do(func() { configFunc() })
	if config.Error() != nil {
		log.Println(config.Error())
		http.Error(w, config.Error().Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var span *trace.Span

	httpFormat := &propagation.HTTPFormat{}
	sc, ok := httpFormat.SpanContextFromRequest(r)
	if ok {
		ctx, span = trace.StartSpanWithRemoteParent(ctx, "weather-frontend", sc,
			trace.WithSampler(trace.AlwaysSample()),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()
	}

	event := r.FormValue("event")
	if event == "" {
		event = "GopherCon"
	}

	apiUrl := "https://us-central1-hightowerlabs.cloudfunctions.net/weather-api"

	u := fmt.Sprintf("%s/api?event=%s", apiUrl, url.QueryEscape(event))

	request, err := http.NewRequest("GET", u, nil)
	if err != nil {
		log.Println("Error calling the weather api: " + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	request = request.WithContext(ctx)

	response, err := httpClient.Do(request)
	if err != nil {
		log.Println("Error calling the weather api: " + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var weatherResponse Weather
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer response.Body.Close()

	if err := json.Unmarshal(body, &weatherResponse); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	events := Events{
		Event{"GopherCon", false},
		Event{"Florida Golang", false},
		Event{"Go Northwest", false},
		Event{"GothamGo", false},
		Event{"CapitalGo", false},
		Event{"Gopherpalooza", false},
	}

	data := struct {
		Event       string
		Location    string
		Temperature int
		Events      Events
	}{
		weatherResponse.Event,
		weatherResponse.Location,
		weatherResponse.Temperature,
		events,
	}

	var html strings.Builder

	if err := htmlTemplate.Execute(&html, data); err != nil {
		log.Println(err)
		http.Error(w, "Unable to load the page", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, html.String())
}

func defaultConfigFunc() {
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

	t := template.New("index.html")
	t, err = t.ParseFiles("static/index.html")
	if err != nil {
		config.err = err
		return
	}

	htmlTemplate = t

	httpClient = &http.Client{
		Transport: &ochttp.Transport{
			Propagation:    &propagation.HTTPFormat{},
			FormatSpanName: func(r *http.Request) string { return "weather-api" },
		},
	}
}
