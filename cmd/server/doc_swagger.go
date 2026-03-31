// General OpenAPI metadata for swag code generation.
//
//go:generate go run github.com/swaggo/swag/cmd/swag@latest init -g doc_swagger.go -o ../../api/docs --parseGoList=false -d .,../../internal/api,../../internal/api/handler,../../internal/api/apidocs,../../internal/domain
package main

// @title Open Streamer API
// @version 1.0
// @description REST API for live stream configuration, ingest/publish lifecycle, DVR recordings, and event hooks.
// @BasePath /
