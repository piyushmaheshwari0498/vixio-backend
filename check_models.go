package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/google/generative-ai-go/genai"
    "github.com/joho/godotenv"
    "google.golang.org/api/iterator"
    "google.golang.org/api/option"
)

func main() {
    godotenv.Load()
    ctx := context.Background()
    apiKey := os.Getenv("GEMINI_API_KEY")

    client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    iter := client.ListModels(ctx)
    fmt.Println("--- AVAILABLE MODELS FOR YOU ---")
    for {
        m, err := iter.Next()
        if err == iterator.Done {
            break
        }
        if err != nil {
            log.Fatal(err)
        }
        // Only show models that can Generate Content
        if m.SupportedGenerationMethods != nil {
            for _, method := range m.SupportedGenerationMethods {
                if method == "generateContent" {
                    fmt.Println(m.Name)
                }
            }
        }
    }
}