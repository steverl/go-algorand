// Package private provides primitives to interact the openapi HTTP API.
//
// Code generated by github.com/algorand/oapi-codegen DO NOT EDIT.
package private

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"github.com/algorand/oapi-codegen/pkg/runtime"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v4"
	"net/http"
	"strings"
)

// ServerInterface represents all server handlers.
type ServerInterface interface {
	// Aborts a catchpoint catchup.
	// (DELETE /v2/catchup/{catchpoint})
	AbortCatchup(ctx echo.Context, catchpoint string) error
	// Starts a catchpoint catchup.
	// (POST /v2/catchup/{catchpoint})
	StartCatchup(ctx echo.Context, catchpoint string) error

	// (POST /v2/register-participation-keys/{address})
	RegisterParticipationKeys(ctx echo.Context, address string, params RegisterParticipationKeysParams) error

	// (POST /v2/shutdown)
	ShutdownNode(ctx echo.Context, params ShutdownNodeParams) error
}

// ServerInterfaceWrapper converts echo contexts to parameters.
type ServerInterfaceWrapper struct {
	Handler ServerInterface
}

// AbortCatchup converts echo context to params.
func (w *ServerInterfaceWrapper) AbortCatchup(ctx echo.Context) error {

	validQueryParams := map[string]bool{
		"pretty": true,
	}

	// Check for unknown query parameters.
	for name, _ := range ctx.QueryParams() {
		if _, ok := validQueryParams[name]; !ok {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unknown parameter detected: %s", name))
		}
	}

	var err error
	// ------------- Path parameter "catchpoint" -------------
	var catchpoint string

	err = runtime.BindStyledParameter("simple", false, "catchpoint", ctx.Param("catchpoint"), &catchpoint)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter catchpoint: %s", err))
	}

	ctx.Set("api_key.Scopes", []string{""})

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.AbortCatchup(ctx, catchpoint)
	return err
}

// StartCatchup converts echo context to params.
func (w *ServerInterfaceWrapper) StartCatchup(ctx echo.Context) error {

	validQueryParams := map[string]bool{
		"pretty": true,
	}

	// Check for unknown query parameters.
	for name, _ := range ctx.QueryParams() {
		if _, ok := validQueryParams[name]; !ok {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unknown parameter detected: %s", name))
		}
	}

	var err error
	// ------------- Path parameter "catchpoint" -------------
	var catchpoint string

	err = runtime.BindStyledParameter("simple", false, "catchpoint", ctx.Param("catchpoint"), &catchpoint)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter catchpoint: %s", err))
	}

	ctx.Set("api_key.Scopes", []string{""})

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.StartCatchup(ctx, catchpoint)
	return err
}

// RegisterParticipationKeys converts echo context to params.
func (w *ServerInterfaceWrapper) RegisterParticipationKeys(ctx echo.Context) error {

	validQueryParams := map[string]bool{
		"pretty":           true,
		"fee":              true,
		"key-dilution":     true,
		"round-last-valid": true,
		"no-wait":          true,
	}

	// Check for unknown query parameters.
	for name, _ := range ctx.QueryParams() {
		if _, ok := validQueryParams[name]; !ok {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unknown parameter detected: %s", name))
		}
	}

	var err error
	// ------------- Path parameter "address" -------------
	var address string

	err = runtime.BindStyledParameter("simple", false, "address", ctx.Param("address"), &address)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter address: %s", err))
	}

	ctx.Set("api_key.Scopes", []string{""})

	// Parameter object where we will unmarshal all parameters from the context
	var params RegisterParticipationKeysParams
	// ------------- Optional query parameter "fee" -------------
	if paramValue := ctx.QueryParam("fee"); paramValue != "" {

	}

	err = runtime.BindQueryParameter("form", true, false, "fee", ctx.QueryParams(), &params.Fee)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter fee: %s", err))
	}

	// ------------- Optional query parameter "key-dilution" -------------
	if paramValue := ctx.QueryParam("key-dilution"); paramValue != "" {

	}

	err = runtime.BindQueryParameter("form", true, false, "key-dilution", ctx.QueryParams(), &params.KeyDilution)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter key-dilution: %s", err))
	}

	// ------------- Optional query parameter "round-last-valid" -------------
	if paramValue := ctx.QueryParam("round-last-valid"); paramValue != "" {

	}

	err = runtime.BindQueryParameter("form", true, false, "round-last-valid", ctx.QueryParams(), &params.RoundLastValid)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter round-last-valid: %s", err))
	}

	// ------------- Optional query parameter "no-wait" -------------
	if paramValue := ctx.QueryParam("no-wait"); paramValue != "" {

	}

	err = runtime.BindQueryParameter("form", true, false, "no-wait", ctx.QueryParams(), &params.NoWait)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter no-wait: %s", err))
	}

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.RegisterParticipationKeys(ctx, address, params)
	return err
}

// ShutdownNode converts echo context to params.
func (w *ServerInterfaceWrapper) ShutdownNode(ctx echo.Context) error {

	validQueryParams := map[string]bool{
		"pretty":  true,
		"timeout": true,
	}

	// Check for unknown query parameters.
	for name, _ := range ctx.QueryParams() {
		if _, ok := validQueryParams[name]; !ok {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unknown parameter detected: %s", name))
		}
	}

	var err error

	ctx.Set("api_key.Scopes", []string{""})

	// Parameter object where we will unmarshal all parameters from the context
	var params ShutdownNodeParams
	// ------------- Optional query parameter "timeout" -------------
	if paramValue := ctx.QueryParam("timeout"); paramValue != "" {

	}

	err = runtime.BindQueryParameter("form", true, false, "timeout", ctx.QueryParams(), &params.Timeout)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid format for parameter timeout: %s", err))
	}

	// Invoke the callback with all the unmarshalled arguments
	err = w.Handler.ShutdownNode(ctx, params)
	return err
}

// RegisterHandlers adds each server route to the EchoRouter.
func RegisterHandlers(router interface {
	CONNECT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	HEAD(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	OPTIONS(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
	TRACE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route
}, si ServerInterface, m ...echo.MiddlewareFunc) {

	wrapper := ServerInterfaceWrapper{
		Handler: si,
	}

	router.DELETE("/v2/catchup/:catchpoint", wrapper.AbortCatchup, m...)
	router.POST("/v2/catchup/:catchpoint", wrapper.StartCatchup, m...)
	router.POST("/v2/register-participation-keys/:address", wrapper.RegisterParticipationKeys, m...)
	router.POST("/v2/shutdown", wrapper.ShutdownNode, m...)

}

// Base64 encoded, gzipped, json marshaled Swagger object
var swaggerSpec = []string{

	"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",
}

// GetSwagger returns the Swagger specification corresponding to the generated code
// in this file.
func GetSwagger() (*openapi3.Swagger, error) {
	zipped, err := base64.StdEncoding.DecodeString(strings.Join(swaggerSpec, ""))
	if err != nil {
		return nil, fmt.Errorf("error base64 decoding spec: %s", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, fmt.Errorf("error decompressing spec: %s", err)
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(zr)
	if err != nil {
		return nil, fmt.Errorf("error decompressing spec: %s", err)
	}

	swagger, err := openapi3.NewSwaggerLoader().LoadSwaggerFromData(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("error loading Swagger: %s", err)
	}
	return swagger, nil
}
