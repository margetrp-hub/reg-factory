package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

func TestRequestsDoNotBecomeTaskEvents(t *testing.T) {
	st := testdatabase.Open(t, t.TempDir())
	previous := log.Writer()
	log.SetOutput(&store.RuntimeLog{Store: st})
	t.Cleanup(func() { log.SetOutput(previous) })
	var access bytes.Buffer
	router := gin.New()
	router.Use(requestLogger(&access))
	router.Any("/api/*path", func(c *gin.Context) {
		switch c.Param("path") {
		case "/unavailable":
			c.Status(http.StatusServiceUnavailable)
		case "/unauthorized":
			c.Status(http.StatusUnauthorized)
		default:
			c.Status(http.StatusOK)
		}
	})
	router.POST("/internal/work/v1/inventory/library", func(c *gin.Context) { c.Status(http.StatusOK) })
	for i := 0; i < 1000; i++ {
		for _, path := range []string{"/api/tasks", "/api/session", "/api/apps/mail/pool", "/api/logs", "/internal/work/v1/inventory/library"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
			if w.Code != http.StatusOK {
				t.Fatal(path, w.Code)
			}
		}
	}
	if access.Len() != 0 {
		t.Fatal("successful polling emitted access logs")
	}
	for _, path := range []string{"/api/unavailable", "/api/unauthorized"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path+"?token=private-query", nil))
	}
	if !strings.Contains(access.String(), "503") || !strings.Contains(access.String(), "401") || strings.Contains(access.String(), "private-query") {
		t.Fatal("HTTP failures must remain in container diagnostics without query credentials", access.String())
	}
	events, err := st.TailEvents("", 100)
	if err != nil || len(events) != 0 {
		t.Fatal("HTTP traffic entered task events", len(events), err)
	}
	if err := st.AppendEvent("real-task", "stderr", "mailbox authorization rejected"); err != nil {
		t.Fatal(err)
	}
	log.Print("[mail] source connection failed")
	events, err = st.TailEvents("", 100)
	if err != nil || len(events) != 2 || events[0].TaskID != "real-task" || !strings.Contains(events[1].Line, "source connection failed") {
		t.Fatal("business evidence or service failures were lost", events, err)
	}
}
