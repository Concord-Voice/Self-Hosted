package requestid

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Canonical returns the lowercase, hyphenated spelling of a UUID.
func Canonical(raw string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Param canonicalizes a Gin route parameter without writing a response.
func Param(c *gin.Context, name string) (string, error) {
	return Canonical(c.Param(name))
}
