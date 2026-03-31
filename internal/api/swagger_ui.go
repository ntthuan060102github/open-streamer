package api

import (
	"net/http"

	"github.com/swaggo/swag"
)

func serveSwaggerJSON(w http.ResponseWriter, _ *http.Request) {
	doc, err := swag.ReadDoc()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(doc))
}

func serveSwaggerIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const page = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8"/>
  <title>Open Streamer API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui.css" crossorigin="anonymous"/>
  <style>body { margin: 0; } #swagger-ui { max-width: 1460px; margin: 0 auto; }</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui-bundle.js" crossorigin="anonymous"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/swagger/doc.json",
      dom_id: '#swagger-ui',
      deepLinking: true
    });
  </script>
</body>
</html>`
	_, _ = w.Write([]byte(page))
}
