package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

const (
	OpenAIBase   = "https://api.openai.com"
	NebiusBase   = "https://api.studio.nebius.ai"
	DeepSeekBase = "https://api.deepseek.com"
)

var httpClient = &http.Client{
	Timeout: 720 * time.Second,
}

func main() {
	app := fiber.New(fiber.Config{
		ReadTimeout:  720 * time.Second,
		WriteTimeout: 720 * time.Second,
		BodyLimit:    50 * 1024 * 1024, // 50MB
	})

	// Middleware
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} - ${method} ${path} ${latency}\n",
	}))

	// Auth middleware
	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	if authToken == "" {
		log.Fatal("PROXY_AUTH_TOKEN must be set")
	}

	app.Use(func(c *fiber.Ctx) error {
		token := c.Get("X-Proxy-Auth")
		if token != authToken {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "Unauthorized",
			})
		}
		return c.Next()
	})

	// Health check
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// OpenAI routes
	app.All("/openai/*", proxyHandler(OpenAIBase, "OPENAI_API_KEY"))

	// Nebius routes
	app.All("/nebius/*", proxyHandler(NebiusBase, "NEBIUS_API_KEY"))

	// DeepSeek routes
	app.All("/deepseek/*", proxyHandler(DeepSeekBase, "DEEPSEEK_API_KEY"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("LLM Proxy starting on port %s", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

func proxyHandler(targetBase, apiKeyEnv string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Получаем путь после префикса (например /openai/v1/chat/completions -> /v1/chat/completions)
		path := c.Params("*")
		targetURL := targetBase + "/" + path

		apiKey := os.Getenv(apiKeyEnv)
		if apiKey == "" {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": apiKeyEnv + " not configured",
			})
		}

		// Создаём запрос к целевому API
		req, err := http.NewRequestWithContext(
			context.Background(),
			c.Method(),
			targetURL,
			bytes.NewReader(c.Body()),
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to create request: " + err.Error(),
			})
		}

		// Копируем заголовки (кроме Host и Authorization)
		for k, v := range c.GetReqHeaders() {
			if k == "Host" || k == "Authorization" || k == "X-Proxy-Auth" {
				continue
			}
			for _, val := range v {
				req.Header.Add(k, val)
			}
		}

		// Добавляем API ключ
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		// Проверяем, streaming ли запрос
		isStreaming := strings.Contains(c.Get("Accept"), "text/event-stream")

		// Выполняем запрос
		resp, err := httpClient.Do(req)
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error": "Failed to proxy request: " + err.Error(),
			})
		}
		defer resp.Body.Close()

		// Копируем заголовки ответа
		for k, v := range resp.Header {
			for _, val := range v {
				c.Response().Header.Add(k, val)
			}
		}

		c.Status(resp.StatusCode)

		// Если streaming - передаём построчно
		if isStreaming && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			c.Set("Content-Type", "text/event-stream")
			c.Set("Cache-Control", "no-cache")
			c.Set("Connection", "keep-alive")

			c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
				scanner := bufio.NewScanner(resp.Body)
				for scanner.Scan() {
					line := scanner.Text()
					w.WriteString(line + "\n")
					w.Flush()
				}
			})
			return nil
		}

		// Обычный ответ
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to read response: " + err.Error(),
			})
		}

		return c.Send(body)
	}
}
