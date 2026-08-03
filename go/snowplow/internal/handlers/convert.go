package handlers

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/http/response"
	"sigs.k8s.io/yaml"
)

func Converter() http.Handler {
	return &convertHandler{}
}

var _ http.Handler = (*convertHandler)(nil)

type convertHandler struct{}

// @Summary Convert YAML to JSON or JSON to YAML
// @Description This endpoint converts YAML to JSON or JSON to YAML based on the "Content-Type" header.
// @ID convert
// @Accept application/json, application/x-yaml, text/yaml
// @Produce application/json, application/x-yaml
// @Param data body string true "Input data in YAML or JSON format"
// @Success 200 {string} string "Converted output in the requested format"
// @Failure 400 {object} response.Status "Bad request, invalid input"
// @Failure 406 {object} response.Status "Unsupported 'Accept' header"
// @Failure 500 {object} response.Status "Internal server error"
// @Router /convert [post]
func (r *convertHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		response.MethodNotAllowed(wri, errors.New("only POST method is allowed"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBodySize))
	if err != nil {
		response.InternalError(wri, err)
		return
	}
	defer req.Body.Close()

	contentType := req.Header.Get("Content-Type")
	toJSON := (strings.Contains(contentType, "application/x-yaml") || strings.Contains(contentType, "text/yaml"))
	toYAML := strings.Contains(contentType, "application/json")

	log := xcontext.Logger(req.Context())

	if toJSON {
		log.Debug("converting data to JSON", slog.String("contentType", contentType),
			slog.String("data", truncate(body, 128)))

		dat, err := yaml.YAMLToJSON(body)
		if err != nil {
			response.BadRequest(wri, fmt.Errorf("failed to encode JSON: %w", err))
			return
		}

		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		wri.Write(dat)

		return
	}

	if toYAML {
		log.Debug("converting data to YAML", slog.String("contentType", contentType),
			slog.String("data", truncate(body, 128)))

		dat, err := yaml.JSONToYAML(body)
		if err != nil {
			response.BadRequest(wri, fmt.Errorf("failed to convert JSON to YAML: %w", err))
			return
		}

		wri.Header().Set("Content-Type", "application/x-yaml")
		wri.WriteHeader(http.StatusOK)
		wri.Write(dat)
		return
	}

	response.NotAcceptable(wri,
		fmt.Errorf("unsupported content type '%s' use 'application/json' or 'application/x-yaml'", contentType))
}

func truncate(data []byte, limit int) string {
	str := string(data) // Converte i byte in stringa

	// Conta i caratteri UTF-8 (rune)
	if utf8.RuneCountInString(str) <= limit {
		return str
	}

	// Converte la stringa in slice di rune per evitare di troncare caratteri multibyte
	runes := []rune(str)

	return string(runes[:limit]) + "..."
}
