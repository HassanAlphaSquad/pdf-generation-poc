package main

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := &Config{
		Port:              "8080",
		MaxConcurrentJobs: 4,
		JobTimeout:        60 * time.Second,
		CacheEnabled:      false,
		CacheTTL:          10 * time.Minute,
		MaxFileSize:       10 << 20, // 10MB
		MongoURI:          "mongodb://localhost:27017",
		MongoDB:           "pdf_service",
		RedisAddr:         "localhost:6379",
		ChromeExecutable:  "/usr/bin/chromium-browser",
		TempDir:           "/tmp",
		RateLimitRequests: 10,
		RateLimitWindow:   time.Second,
	}

	service, err := NewPDFGeneratorService(cfg)
	if err != nil {
		log.Fatalf("Failed to start service: %v", err)
	}

	r := gin.Default()

	// Example route: sync PDF generation
	r.POST("/api/v1/pdf/generate", func(c *gin.Context) {
		var req PDFRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		pdfData, err := service.generatePDFWithChrome(req.HTML, req.Options)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		c.Data(200, "application/pdf", pdfData)
	})

	log.Printf("PDF Service running on :%s", cfg.Port)
	r.Run(":" + cfg.Port)
}
