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
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/trace"

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
		ctx, span = trace.StartSpanWithRemoteParent(ctx, "weather-api", sc,
			trace.WithSampler(trace.AlwaysSample()),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()
	}

	event := r.FormValue("event")
	if event == "" {
		log.Println("empty event parameter")
		http.Error(w, "missing event query parameter", http.StatusBadRequest)
		return
	}

	weather, err := getWeatherForEvent(ctx, event)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(weather); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func getWeatherForEvent(ctx context.Context, event string) (*Weather, error) {
	_, span := trace.StartSpan(ctx, "cloud-sql")
	span.AddAttributes(
		trace.StringAttribute("cloudsql", "postgres"),
		trace.StringAttribute("schema", "weather"),
	)
	span.Annotate([]trace.Attribute{
		trace.StringAttribute("Query", "SELECT event,location,temperature FROM weather WHERE event = $1"),
	}, "query")

	defer span.End()

	var w Weather

	err := config.db.QueryRow("SELECT event,location,temperature FROM weather WHERE event = $1", event).Scan(
		&w.Event, &w.Location, &w.Temperature)
	switch {
	case err == sql.ErrNoRows:
		return nil, err
	case err != nil:
		return nil, err
	}

	return &w, nil
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
