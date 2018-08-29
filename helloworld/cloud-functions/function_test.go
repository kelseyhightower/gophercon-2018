package function

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestF(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler := http.HandlerFunc(F)
	handler.ServeHTTP(w, r)

	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("wrong status code: got %v want %v", resp.StatusCode, http.StatusOK)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "Hello, World!\n" {
		t.Errorf("wrong response body: got %v want %v", body, "Hello, World!\n")
	}
}
