// fiber-api: minimal HTTP service using gofiber/fiber v2.
package main

import (
	"log"
	"os"

	"github.com/gofiber/fiber/v2"
)

type todo struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func main() {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	var todos []todo

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"app": "fiber-api", "endpoints": []string{"/health", "/todos"}})
	})
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "count": len(todos)})
	})
	app.Get("/todos", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"todos": todos})
	})
	app.Post("/todos", func(c *fiber.Ctx) error {
		var t todo
		if err := c.BodyParser(&t); err != nil || t.Text == "" {
			return fiber.NewError(fiber.StatusBadRequest, "text required")
		}
		t.ID = len(todos) + 1
		todos = append(todos, t)
		return c.Status(fiber.StatusCreated).JSON(t)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(app.Listen(":" + port))
}
