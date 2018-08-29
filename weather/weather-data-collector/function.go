package function

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sync"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/storage"
	"go.opencensus.io/trace"
	"googlemaps.github.io/maps"

	_ "github.com/lib/pq"
)

var (
	db         *sql.DB
	logger     *logging.Logger
	mapsClient *maps.Client
	once       sync.Once
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

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
	Data []byte `json:"data"`
}

type WeatherEvent struct {
	Event    string `json:"event"`
	Location string `json:"location"`
}

func F(ctx context.Context, m PubSubMessage) error {
	once.Do(func() {
		if err := configFunc(); err != nil {
			panic(err)
		}
	})

	defer logger.Flush()

	var e WeatherEvent
	if err := json.Unmarshal(m.Data, &e); err != nil {
		return err
	}

	ctx, span := trace.StartSpan(ctx, "weather-data-collector")
	defer span.End()

	lat, lng, err := geoFromLocation(ctx, e.Location)
	if err != nil {
		return err
	}

	temperature, err := getTemperature(ctx, lat, lng)
	if err != nil {
		return err
	}

	return updateDatabase(ctx, e.Event, e.Location, temperature)
}

func updateDatabase(ctx context.Context, event string, location string, temperature int) error {
	ctx, span := trace.StartSpan(ctx, "cloud-sql")
	defer span.End()

	logger.Log(logging.Entry{
		Payload:  fmt.Sprintf("setting temperature for %s in %s to %d", event, location, temperature),
		Severity: logging.Info,
	})

	_, err := db.Exec(query, event, location, temperature)
	return err
}

func getTemperature(ctx context.Context, lat, lng float64) (int, error) {
	ctx, span := trace.StartSpan(ctx, "api.weather.gov/points/forecast/hourly")
	defer span.End()

	u := fmt.Sprintf("https://api.weather.gov/points/%.4f,%.4f/forecast/hourly", lat, lng)

	logger.Log(logging.Entry{
		Payload:  fmt.Sprintf("retrieving weather data for (%.4f,%.4f)", lat, lng),
		Severity: logging.Info,
	})

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

	r, err := mapsClient.FindPlaceFromText(context.Background(),
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

	r, err := mapsClient.PlaceDetails(ctx, &maps.PlaceDetailsRequest{PlaceID: id})
	if err != nil {
		return 0, 0, err
	}

	return r.Geometry.Location.Lat, r.Geometry.Location.Lng, nil
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

	ctx := context.Background()

	bucketName := os.Getenv("CONFIGURATION_BUCKET_NAME")
	if bucketName == "" {
		return fmt.Errorf("CONFIGURATION_BUCKET_NAME environment variable unset or missing")
	}

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}

	// Setup the Google maps API client
	apiKey, err := objectToString(storageClient, bucketName, "maps-api-key")
	if err != nil {
		return err
	}

	mapsClient, err = maps.NewClient(maps.WithAPIKey(apiKey))
	if err != nil {
		return err
	}

	// Fetch the Cloud SQL credentials and make them
	// available to the lib/pq database driver.
	password, err := objectToString(storageClient, bucketName, "password")
	if err != nil {
		return err
	}

	clientCert, err := objectToTempFile(storageClient, bucketName, "client.pem")
	if err != nil {
		return err
	}

	clientKey, err := objectToTempFile(storageClient, bucketName, "client.key")
	if err != nil {
		return err
	}

	serverCert, err := objectToTempFile(storageClient, bucketName, "server.pem")
	if err != nil {
		return err
	}

	// Use environment variables to configure the database connection
	// parameters because it allows us to leverage both run-time and
	// deploy time configuration.
	err = os.Setenv("PGSSLCERT", clientCert)
	if err != nil {
		return err
	}

	err = os.Setenv("PGSSLKEY", clientKey)
	if err != nil {
		return err
	}

	err = os.Setenv("PGSSLROOTCERT", serverCert)
	if err != nil {
		return err
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

	db, err = sql.Open("postgres", dsn)
	if err != nil {
		return err
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

	return nil
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
