package handler

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed favicon.png
var faviconPNG []byte

const cloudLandingPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" media="(prefers-color-scheme: light)" content="#F7F7F4">
<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#141512">
<meta name="description" content="MaidKit Cloud is up and running.">
<title>MaidKit Cloud</title>
<link rel="icon" type="image/png" href="/favicon.png">
<style>
  :root {
    color-scheme: light dark;
    --paper: #F7F7F4;
    --ink: #1E1F1C;
    --up: #447A3B;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --paper: #141512;
      --ink: #E8E9E4;
      --up: #8FC48E;
    }
  }
  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    margin: 0;
    display: grid;
    place-items: center;
    padding: 24px;
    background: var(--paper);
    color: var(--ink);
    font: 500 clamp(1rem, 1.2vw + 0.75rem, 1.35rem)/1.7 ui-monospace, "SF Mono", "Cascadia Mono", "JetBrains Mono", Menlo, Consolas, monospace;
    -webkit-font-smoothing: antialiased;
  }
  main {
    max-width: 56ch;
    text-align: center;
  }
  h1 {
    margin: 0;
    font-size: inherit;
    font-weight: 500;
    letter-spacing: 0.01em;
    text-wrap: balance;
  }
  h1::before {
    content: "";
    display: inline-block;
    width: 0.55em;
    height: 0.55em;
    margin-right: 0.45em;
    vertical-align: 0.07em;
    background: var(--up);
    animation: breathe 3s ease-in-out infinite;
  }
  @keyframes breathe {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.4; }
  }
  @media (prefers-reduced-motion: reduce) {
    h1::before { animation: none; }
  }
</style>
</head>
<body>
  <main>
    <h1>MaidKit Cloud is up and running</h1>
  </main>
</body>
</html>
`

func serveFavicon(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, "image/png", faviconPNG)
}
