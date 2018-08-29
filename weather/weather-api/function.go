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
	"contrib.go.opencensus.io/exporter/stackdriver/propagation"
	"go.opencensus.io/trace"

	_ "github.com/lib/pq"
)

var (
	db     *sql.DB
	logger *logging.Logger
	once   sync.Once
)

// configFunc sets the global configuration; it's overridden in tests.
var configFunc = defaultConfigFunc

type Weather struct {
	Event       string
	Location    string
	Temperature int
}

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
		ctx, span = trace.StartSpanWithRemoteParent(ctx, "weather-api", sc,
			trace.WithSampler(trace.AlwaysSample()),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()
	}

	event := r.FormValue("event")
	if event == "" {
		logger.Log(logging.Entry{
			Payload:  "missing event query parameter",
			Severity: logging.Error,
		})
		http.Error(w, "missing event query parameter", http.StatusBadRequest)
		return
	}

	weather, err := getWeatherForEvent(ctx, event)
	if err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(weather); err != nil {
		logger.Log(logging.Entry{
			Payload:  err.Error(),
			Severity: logging.Error,
		})
		http.Error(w, "", http.StatusInternalServerError)
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

	err := db.QueryRow("SELECT event,location,temperature FROM weather WHERE event = $1", event).Scan(
		&w.Event, &w.Location, &w.Temperature)
	switch {
	case err == sql.ErrNoRows:
		return nil, err
	case err != nil:
		return nil, err
	}

	return &w, nil
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
