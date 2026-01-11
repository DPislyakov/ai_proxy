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
	OpenAIBase    = "https://api.openai.com"
	NebiusBase    = "https://api.studio.nebius.ai"
	DeepSeekBase  = "https://api.deepseek.com"
	AnthropicBase = "https://api.anthropic.com"
)

var httpClient = &http.Client{
	Timeout: 720 * time.Second,
}

func main() {
	app := fiber.New(fiber.Config{
		ReadTimeout:       720 * time.Second,
		WriteTimeout:      720 * time.Second,
		IdleTimeout:       720 * time.Second,
		BodyLimit:         50 * 1024 * 1024, // 50MB
		StreamRequestBody: true,
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
	app.All("/openai/*", proxyHandler(OpenAIBase, "OPENAI_API_KEY", "openai"))

	// Nebius routes
	app.All("/nebius/*", proxyHandler(NebiusBase, "NEBIUS_API_KEY", "nebius"))

	// DeepSeek routes
	app.All("/deepseek/*", proxyHandler(DeepSeekBase, "DEEPSEEK_API_KEY", "deepseek"))

	// Anthropic routes
	app.All("/anthropic/*", proxyHandler(AnthropicBase, "ANTHROPIC_API_KEY", "anthropic"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("LLM Proxy starting on port %s", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

func proxyHandler(targetBase, apiKeyEnv, provider string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Получаем путь после префикса
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

		// Копируем заголовки
		for k, v := range c.GetReqHeaders() {
			if k == "Host" || k == "Authorization" || k == "X-Proxy-Auth" || k == "X-Api-Key" {
				continue
			}
			for _, val := range v {
				req.Header.Add(k, val)
			}
		}

		// Добавляем API ключ в зависимости от провайдера
		if provider == "anthropic" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
			// Добавляем beta header для structured outputs если нужно
			if c.Get("anthropic-beta") != "" {
				req.Header.Set("anthropic-beta", c.Get("anthropic-beta"))
			}
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
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

		// Если streaming - передаём SSE корректно
		if isStreaming && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			c.Set("Content-Type", "text/event-stream")
			c.Set("Cache-Control", "no-cache")
			c.Set("Connection", "keep-alive")
			c.Set("X-Accel-Buffering", "no") // Отключаем буферизацию nginx

			c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
				reader := bufio.NewReader(resp.Body)
				var bytesWritten int64

				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						if err != io.EOF {
							log.Printf("Stream read error: %v (after %d bytes)", err, bytesWritten)
						}
						break
					}

					// Пишем строку как есть (включая \n)
					n, err := w.WriteString(line)
					if err != nil {
						log.Printf("Stream write error: %v", err)
						break
					}
					bytesWritten += int64(n)

					// КРИТИЧНО: flush после каждой строки для SSE
					if err := w.Flush(); err != nil {
						log.Printf("Stream flush error: %v", err)
						break
					}

					// Дополнительный flush для пустых строк (разделитель событий SSE)
					if strings.TrimSpace(line) == "" {
						w.Flush()
					}
				}

				log.Printf("Stream completed: %d bytes written", bytesWritten)
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
