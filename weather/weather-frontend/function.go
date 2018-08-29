package function

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"text/template"

	"cloud.google.com/go/logging"
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/trace"
)

var (
	htmlTemplate *template.Template
	httpClient   *http.Client
	logger       *logging.Logger
	once         sync.Once
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

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
	once.Do(func() {
		if err := configFunc(); err != nil {
			panic(err)
		}
	})

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
		logger.Log(logging.Entry{
			Payload:  "Error calling the weather api: " + err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	request = request.WithContext(ctx)

	response, err := httpClient.Do(request)
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  "Error calling the weather api: " + err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var weatherResponse Weather
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer response.Body.Close()

	if err := json.Unmarshal(body, &weatherResponse); err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
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
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, "Unable to load the page", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, html.String())
}

func defaultConfigFunc() error {
	var err error

	if err := EnableStackdriverTrace(); err != nil {
		return err
	}

	logger, err = NewStackdriverLogger()
	if err != nil {
		return err
	}

	t := template.New("index.html")
	t, err = t.ParseFiles("static/index.html")
	if err != nil {
		return err
	}

	htmlTemplate = t

	httpClient = &http.Client{
		Transport: &ochttp.Transport{
			Propagation:    &propagation.HTTPFormat{},
			FormatSpanName: func(r *http.Request) string { return "weather-api" },
		},
	}

	return nil
}
