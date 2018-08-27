package function

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"

	"cloud.google.com/go/storage"
	"contrib.go.opencensus.io/exporter/stackdriver"
	"go.opencensus.io/trace"
	"googlemaps.github.io/maps"

	_ "github.com/lib/pq"
)

func init() {
	config = &configuration{}
}

var (
	config *configuration
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

type configuration struct {
	db   *sql.DB
	mc   *maps.Client
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

type HourlyForecast struct {
	Type       string
	Properties Properties
}

type Properties struct {
	Periods []Period
}

type Period struct {
	Temperature int
}

type PubSubMessage struct {
	Event    string `json:"event"`
	Location string `json:"location"`
}

func F(ctx context.Context, m PubSubMessage) error {
	config.once.Do(func() { configFunc() })

	ctx, span := trace.StartSpan(ctx, "weather-data-collector")
	defer span.End()

	if config.Error() != nil {
		return config.Error()
	}

	lat, lng, err := geoFromLocation(ctx, m.Location)
	if err != nil {
		return err
	}

	temperature, err := getTemperature(ctx, lat, lng)
	if err != nil {
		return err
	}

	return updateDatabase(ctx, m.Event, m.Location, temperature)
}

func updateDatabase(ctx context.Context, event string, location string, temperature int) error {
	ctx, span := trace.StartSpan(ctx, "cloud-sql")
	defer span.End()

	log.Printf("setting temperature for %s in %s to %d", event, location, temperature)
	_, err := config.db.Exec(query, event, location, temperature)
	return err
}

func getTemperature(ctx context.Context, lat, lng float64) (int, error) {
	ctx, span := trace.StartSpan(ctx, "api.weather.gov/points/forecast/hourly")
	defer span.End()

	u := fmt.Sprintf("https://api.weather.gov/points/%.4f,%.4f/forecast/hourly", lat, lng)
	log.Printf("retrieving weather data for (%.4f,%.4f)", lat, lng)

	request, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}

	request.Header.Add("User-Agent", "Weather Function 1.0")
	request.Header.Add("Accept", "application/geo+json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}

	data, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}

	response.Body.Close()

	var forecast HourlyForecast
	if err := json.Unmarshal(data, &forecast); err != nil {
		return 0, err
	}

	return forecast.Properties.Periods[0].Temperature, nil
}

func geoFromLocation(ctx context.Context, location string) (float64, float64, error) {
	ctx, span := trace.StartSpan(ctx, "google-maps-api")
	defer span.End()

	id, err := findPlaceIDFromText(ctx, location)
	if err != nil {
		return 0, 0, err
	}

	return getLatLng(ctx, id)
}

func findPlaceIDFromText(ctx context.Context, location string) (string, error) {
	ctx, span := trace.StartSpan(ctx, "google-maps-find-place")
	defer span.End()

	r, err := config.mc.FindPlaceFromText(context.Background(),
		&maps.FindPlaceFromTextRequest{
			Input:     location,
			InputType: maps.FindPlaceFromTextInputTypeTextQuery,
		},
	)
	if err != nil {
		return "", err
	}

	return r.Candidates[0].PlaceID, nil
}

func getLatLng(ctx context.Context, id string) (float64, float64, error) {
	ctx, span := trace.StartSpan(ctx, "google-maps-place-details")
	defer span.End()

	r, err := config.mc.PlaceDetails(ctx, &maps.PlaceDetailsRequest{PlaceID: id})
	if err != nil {
		return 0, 0, err
	}

	return r.Geometry.Location.Lat, r.Geometry.Location.Lng, nil
}

func defaultConfigFunc() {
	ctx := context.Background()

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

	bucketName := os.Getenv("CONFIGURATION_BUCKET_NAME")
	if bucketName == "" {
		config.err = &envError{"CONFIGURATION_BUCKET_NAME"}
		return
	}

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		config.err = err
		return
	}

	// Setup the Google maps API client
	apiKey, err := objectToString(storageClient, bucketName, "maps-api-key")
	if err != nil {
        config.err = err
        return
    }

	mapClient, err := maps.NewClient(maps.WithAPIKey(apiKey))
	if err != nil {
		config.err = err
		return
	}

	config.mc = mapClient

	// Fetch the Cloud SQL credentials and make them
	// available to the lib/pq database driver.
	password, err := objectToString(storageClient, bucketName, "password")
	if err != nil {
        config.err = err
        return
    }

	clientCert, err := objectToTempFile(storageClient, bucketName, "client.pem")
	if err != nil {
		config.err = err
		return
	}

	clientKey, err := objectToTempFile(storageClient, bucketName, "client.key")
	if err != nil {
		config.err = err
		return
	}

	serverCert, err := objectToTempFile(storageClient, bucketName, "server.pem")
	if err != nil {
		config.err = err
		return
	}


	// Use environment variables to configure the database connection
	// parameters because it allows us to leverage both run-time and
	// deploy time configuration.
	err = os.Setenv("PGSSLCERT", clientCert)
	if err != nil {
		config.err = err
		return
	}

	err = os.Setenv("PGSSLKEY", clientKey)
	if err != nil {
		config.err = err
		return
	}

	err = os.Setenv("PGSSLROOTCERT", serverCert)
	if err != nil {
		config.err = err
		return
	}

	// Don't store the decrypted database password in an environment
	// variable to avoid security concerns. Instead store the password
	// in a connection string that will be used to configure the
	// database connection parameters.
	//
	// The `lib/pq` database driver supports configuring database
	// connection parameters using both environment variables and
	// a connection string. Environment variables take lower
	// precedence.
	//
	// See the `lib/pq` docs for more details:
	//
	//     https://godoc.org/github.com/lib/pq
	//
	dsn := fmt.Sprintf("password='%s'", password)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		config.err = err
		return
	}

	// Google Cloud Functions ensures a single request per function
	// invocation. Avoid exhausting database connections by limiting
	// this function to a single database connection.
	//
	// See the cloud functions docs for more details:
	//
	//    https://cloud.google.com/functions/docs/sql
	//
	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(1)

	config.db = db
}

func objectToString(client *storage.Client, bucketName, objectName string) (string, error) {
	ctx := context.Background()
	o, err := client.Bucket(bucketName).Object(objectName).NewReader(ctx)
    if err != nil {
        return "", err
    }
    defer o.Close()

	data, err := ioutil.ReadAll(o)
	if err != nil {
        return "", err
    }

	return string(bytes.TrimSpace(data)), nil
}

func objectToTempFile(client *storage.Client, bucketName, objectName string) (string, error) {
	ctx := context.Background()
	o, err := client.Bucket(bucketName).Object(objectName).NewReader(ctx)
	if err != nil {
		return "", err
	}
	defer o.Close()

	t, err := ioutil.TempFile("", "")
	if err != nil {
		return "", err
	}
	defer t.Close()

	if _, err := io.Copy(t, o); err != nil {
		return "", err
	}

	return t.Name(), nil
}

var query = `INSERT INTO weather (event, location, temperature)
  VALUES ($1, $2, $3)
  ON CONFLICT (event)
  DO UPDATE SET temperature = EXCLUDED.temperature;`
