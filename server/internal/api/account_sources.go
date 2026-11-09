package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) accountSources(c *gin.Context) {
	ref := strings.TrimSpace(c.Query("ref"))
	if ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_ref"})
		return
	}
	items, err := s.st.AccountSources(ref)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account_sources_unavailable"})
		return
	}
	for i := range items {
		s.credentialMetadata(&items[i])
	}
	c.JSON(http.StatusOK, gin.H{"accounts": items, "total": len(items)})
}
