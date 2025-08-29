package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// Configuration struct
type Config struct {
	Port              string        `json:"port"`
	MaxConcurrentJobs int           `json:"max_concurrent_jobs"`
	JobTimeout        time.Duration `json:"job_timeout"`
	CacheEnabled      bool          `json:"cache_enabled"`
	CacheTTL          time.Duration `json:"cache_ttl"`
	MaxFileSize       int64         `json:"max_file_size"`
	MongoURI          string        `json:"mongo_uri"`
	MongoDB           string        `json:"mongo_db"`
	RedisAddr         string        `json:"redis_addr"`
	ChromeExecutable  string        `json:"chrome_executable"`
	TempDir           string        `json:"temp_dir"`
	RateLimitRequests int           `json:"rate_limit_requests"`
	RateLimitWindow   time.Duration `json:"rate_limit_window"`
}

// PDF generation options
type PDFOptions struct {
	Format              string            `json:"format" bson:"format"`
	Orientation         string            `json:"orientation" bson:"orientation"`
	Margin              map[string]string `json:"margin" bson:"margin"`
	PrintBackground     bool              `json:"printBackground" bson:"printBackground"`
	DisplayHeaderFooter bool              `json:"displayHeaderFooter" bson:"displayHeaderFooter"`
	HeaderTemplate      string            `json:"headerTemplate" bson:"headerTemplate"`
	FooterTemplate      string            `json:"footerTemplate" bson:"footerTemplate"`
	Scale               float64           `json:"scale" bson:"scale"`
	PreferCSSPageSize   bool              `json:"preferCSSPageSize" bson:"preferCSSPageSize"`
	Width               string            `json:"width,omitempty" bson:"width,omitempty"`
	Height              string            `json:"height,omitempty" bson:"height,omitempty"`
}

// PDF generation request
type PDFRequest struct {
	HTML    string     `json:"html" binding:"required"`
	Options PDFOptions `json:"options"`
}

// Batch PDF request
type BatchPDFRequest struct {
	Documents []struct {
		HTML    string     `json:"html" binding:"required"`
		Options PDFOptions `json:"options"`
	} `json:"documents" binding:"required"`
	Options PDFOptions `json:"options"`
}

// PDF generation job
type PDFJob struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	HTML        string             `bson:"html"`
	Options     PDFOptions         `bson:"options"`
	Status      string             `bson:"status"` // pending, processing, completed, failed
	Result      []byte             `bson:"result,omitempty"`
	Error       string             `bson:"error,omitempty"`
	CreatedAt   time.Time          `bson:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at"`
	CompletedAt *time.Time         `bson:"completed_at,omitempty"`
	CacheKey    string             `bson:"cache_key"`
}

// Service statistics
type ServiceStats struct {
	ActiveJobs     int     `json:"active_jobs"`
	QueuedJobs     int     `json:"queued_jobs"`
	CompletedJobs  int     `json:"completed_jobs"`
	FailedJobs     int     `json:"failed_jobs"`
	CacheHitRate   float64 `json:"cache_hit_rate"`
	AvgProcessTime float64 `json:"avg_process_time_ms"`
}

// PDF Generator Service
type PDFGeneratorService struct {
	config         *Config
	logger         *zap.Logger
	mongoClient    *mongo.Client
	redisClient    *redis.Client
	jobsCollection *mongo.Collection
	rateLimiter    *rate.Limiter
	activeJobs     sync.Map
	jobQueue       chan *PDFJob
	stats          ServiceStats
	statsMutex     sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
}

// Initialize service
func NewPDFGeneratorService(config *Config) (*PDFGeneratorService, error) {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	service := &PDFGeneratorService{
		config:      config,
		logger:      logger,
		ctx:         ctx,
		cancel:      cancel,
		jobQueue:    make(chan *PDFJob, config.MaxConcurrentJobs*2),
		rateLimiter: rate.NewLimiter(rate.Every(config.RateLimitWindow/time.Duration(config.RateLimitRequests)), config.RateLimitRequests),
	}

	// Initialize MongoDB
	if err := service.initMongoDB(); err != nil {
		return nil, fmt.Errorf("failed to initialize MongoDB: %w", err)
	}

	// Initialize Redis (optional)
	if config.CacheEnabled && config.RedisAddr != "" {
		service.initRedis()
	}

	// Start job processors
	service.startJobProcessors()

	return service, nil
}

// Initialize MongoDB connection
func (s *PDFGeneratorService) initMongoDB() error {
	clientOptions := options.Client().ApplyURI(s.config.MongoURI)
	client, err := mongo.Connect(s.ctx, clientOptions)
	if err != nil {
		return err
	}

	// Test connection
	if err := client.Ping(s.ctx, nil); err != nil {
		return err
	}

	s.mongoClient = client
	s.jobsCollection = client.Database(s.config.MongoDB).Collection("pdf_jobs")

	// Create indexes
	s.createIndexes()

	s.logger.Info("MongoDB connection established")
	return nil
}

// Initialize Redis connection
func (s *PDFGeneratorService) initRedis() {
	s.redisClient = redis.NewClient(&redis.Options{
		Addr: s.config.RedisAddr,
	})

	// Test connection
	_, err := s.redisClient.Ping(s.ctx).Result()
	if err != nil {
		s.logger.Warn("Failed to connect to Redis, caching disabled", zap.Error(err))
		s.redisClient = nil
		s.config.CacheEnabled = false
	} else {
		s.logger.Info("Redis connection established")
	}
}

// Create MongoDB indexes
func (s *PDFGeneratorService) createIndexes() {
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "cache_key", Value: 1}},
			Options: options.Index().SetUnique(false),
		},
		{
			Keys: bson.D{{Key: "status", Value: 1}},
		},
		{
			Keys: bson.D{{Key: "created_at", Value: -1}},
		},
		{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(7 * 24 * 3600), // 7 days
		},
	}

	_, err := s.jobsCollection.Indexes().CreateMany(s.ctx, indexes)
	if err != nil {
		s.logger.Warn("Failed to create indexes", zap.Error(err))
	}
}

// Start job processors
func (s *PDFGeneratorService) startJobProcessors() {
	for i := 0; i < s.config.MaxConcurrentJobs; i++ {
		go s.jobProcessor()
	}
	s.logger.Info("Started job processors", zap.Int("count", s.config.MaxConcurrentJobs))
}

// Job processor worker
func (s *PDFGeneratorService) jobProcessor() {
	for {
		select {
		case job := <-s.jobQueue:
			s.processJob(job)
		case <-s.ctx.Done():
			return
		}
	}
}

// Process a single PDF job
func (s *PDFGeneratorService) processJob(job *PDFJob) {
	s.activeJobs.Store(job.ID.Hex(), job)
	defer s.activeJobs.Delete(job.ID.Hex())

	startTime := time.Now()

	// Update job status
	s.updateJobStatus(job.ID, "processing", "", nil)

	// Check cache first
	if s.config.CacheEnabled && s.redisClient != nil {
		if cached := s.getCachedPDF(job.CacheKey); cached != nil {
			s.updateJobStatus(job.ID, "completed", "", cached)
			s.updateStats("cache_hit", time.Since(startTime))
			return
		}
	}

	// Generate PDF
	pdfData, err := s.generatePDFWithChrome(job.HTML, job.Options)
	if err != nil {
		s.logger.Error("PDF generation failed",
			zap.String("job_id", job.ID.Hex()),
			zap.Error(err))
		s.updateJobStatus(job.ID, "failed", err.Error(), nil)
		s.updateStats("failed", time.Since(startTime))
		return
	}

	// Cache the result
	if s.config.CacheEnabled && s.redisClient != nil {
		s.cachePDF(job.CacheKey, pdfData)
	}

	// Update job with result
	s.updateJobStatus(job.ID, "completed", "", pdfData)
	s.updateStats("completed", time.Since(startTime))
}

// Generate PDF using Chrome headless
func (s *PDFGeneratorService) generatePDFWithChrome(html string, options PDFOptions) ([]byte, error) {
	// Validate and sanitize HTML
	sanitizedHTML := s.sanitizeHTML(html)

	// Create temporary HTML file
	tmpFile, err := s.createTempHTMLFile(sanitizedHTML)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	// Build Chrome command
	args := s.buildChromeArgs(tmpFile, options)

	// Execute Chrome
	ctx, cancel := context.WithTimeout(s.ctx, s.config.JobTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.config.ChromeExecutable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("chrome execution failed: %w, stderr: %s", err, stderr.String())
	}

	pdfData := stdout.Bytes()
	if len(pdfData) == 0 {
		return nil, fmt.Errorf("chrome produced empty PDF output")
	}

	return pdfData, nil
}

// Create temporary HTML file
func (s *PDFGeneratorService) createTempHTMLFile(html string) (string, error) {
	tmpFile, err := os.CreateTemp(s.config.TempDir, "pdf_*.html")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(html); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}

	return tmpFile.Name(), nil
}

// Build Chrome command arguments
func (s *PDFGeneratorService) buildChromeArgs(htmlFile string, options PDFOptions) []string {
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-features=TranslateUI",
		"--disable-ipc-flooding-protection",
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--virtual-time-budget=25000",
	}

	// PDF-specific options
	args = append(args, "--print-to-pdf=-") // Output to stdout

	if options.Format != "" {
		args = append(args, fmt.Sprintf("--print-to-pdf-format=%s", options.Format))
	}

	if options.Orientation == "landscape" {
		args = append(args, "--print-to-pdf-landscape")
	}

	if options.PrintBackground {
		args = append(args, "--print-to-pdf-background")
	}

	// Margins
	if margin := options.Margin; len(margin) > 0 {
		if top, ok := margin["top"]; ok {
			args = append(args, fmt.Sprintf("--print-to-pdf-margin-top=%s", s.convertMargin(top)))
		}
		if bottom, ok := margin["bottom"]; ok {
			args = append(args, fmt.Sprintf("--print-to-pdf-margin-bottom=%s", s.convertMargin(bottom)))
		}
		if left, ok := margin["left"]; ok {
			args = append(args, fmt.Sprintf("--print-to-pdf-margin-left=%s", s.convertMargin(left)))
		}
		if right, ok := margin["right"]; ok {
			args = append(args, fmt.Sprintf("--print-to-pdf-margin-right=%s", s.convertMargin(right)))
		}
	}

	// Custom page size
	if options.Width != "" && options.Height != "" {
		args = append(args, fmt.Sprintf("--print-to-pdf-page-width=%s", options.Width))
		args = append(args, fmt.Sprintf("--print-to-pdf-page-height=%s", options.Height))
	}

	// Scale
	if options.Scale > 0 && options.Scale != 1.0 {
		args = append(args, fmt.Sprintf("--print-to-pdf-scale=%.2f", options.Scale))
	}

	args = append(args, fmt.Sprintf("file://%s", htmlFile))
	return args
}

// Convert margin string (e.g., "2cm" to inches for Chrome)
func (s *PDFGeneratorService) convertMargin(margin string) string {
	// Chrome expects margins in inches
	if strings.HasSuffix(margin, "cm") {
		if val, err := strconv.ParseFloat(strings.TrimSuffix(margin, "cm"), 64); err == nil {
			return fmt.Sprintf("%.2f", val/2.54) // cm to inches
		}
	}
	if strings.HasSuffix(margin, "mm") {
		if val, err := strconv.ParseFloat(strings.TrimSuffix(margin, "mm"), 64); err == nil {
			return fmt.Sprintf("%.2f", val/25.4) // mm to inches
		}
	}
	if strings.HasSuffix(margin, "in") {
		return strings.TrimSuffix(margin, "in")
	}
	// Default to treating as inches
	return margin
}

// Sanitize HTML content
func (s *PDFGeneratorService) sanitizeHTML(html string) string {
	// Basic HTML sanitization - remove potentially dangerous scripts
	scriptRegex := regexp.MustCompile(`<script[^>]*>.*?</script>`)
	html = scriptRegex.ReplaceAllString(html, "")

	// Remove javascript: protocols
	jsRegex := regexp.MustCompile(`javascript:`)
	html = jsRegex.ReplaceAllString(html, "")

	// Remove on* event handlers
	onEventRegex := regexp.MustCompile(`\s+on\w+\s*=\s*["'][^"']*["']`)
	html = onEventRegex.ReplaceAllString(html, "")

	return html
}

// Generate cache key
func (s *PDFGeneratorService) generateCacheKey(html string, options PDFOptions) string {
	hasher := sha256.New()
	hasher.Write([]byte(html))
	optionsBytes, _ := json.Marshal(options)
	hasher.Write(optionsBytes)
	return hex.EncodeToString(hasher.Sum(nil))
}

// Get cached PDF
func (s *PDFGeneratorService) getCachedPDF(cacheKey string) []byte {
	if s.redisClient == nil {
		return nil
	}

	cached, err := s.redisClient.Get(s.ctx, "pdf:"+cacheKey).Bytes()
	if err != nil {
		return nil
	}

	return cached
}

// Cache PDF
func (s *PDFGeneratorService) cachePDF(cacheKey string, data []byte) {
	if s.redisClient == nil {
		return
	}

	err := s.redisClient.Set(s.ctx, "pdf:"+cacheKey, data, s.config.CacheTTL).Err()
	if err != nil {
		s.logger.Warn("Failed to cache PDF", zap.Error(err))
	}
}

// Update job status in MongoDB
func (s *PDFGeneratorService) updateJobStatus(jobID primitive.ObjectID, status string, errorMsg string, result []byte) {
	update := bson.M{
		"$set": bson.M{
			"status":     status,
			"updated_at": time.Now(),
		},
	}

	if errorMsg != "" {
		update["$set"].(bson.M)["error"] = errorMsg
	}

	if result != nil {
		update["$set"].(bson.M)["result"] = result
	}

	if status == "completed" || status == "failed" {
		now := time.Now()
		update["$set"].(bson.M)["completed_at"] = &now
	}

	_, err := s.jobsCollection.UpdateByID(s.ctx, jobID, update)
	if err != nil {
		s.logger.Error("Failed to update job status", zap.Error(err))
	}
}

// Update service statistics
func (s *PDFGeneratorService) updateStats(operation string, duration time.Duration) {
	s.statsMutex.Lock()
	defer s.statsMutex.Unlock()

	switch operation {
	case "completed":
		s.stats.CompletedJobs++
	case "failed":
		s.stats.FailedJobs++
	case "cache_hit":
		s.stats.CompletedJobs++
		// Update cache hit rate
	}

	// Update average processing time
	if operation == "completed" || operation == "failed" {
		totalJobs := s.stats.CompletedJobs + s.stats.FailedJobs
		if totalJobs > 0 {
			s.stats.AvgProcessTime = (s.stats.AvgProcessTime*float64(totalJobs-1) + float64(duration.Milliseconds())) / float64(totalJobs)
		}
	}
}

// Get service statistics
func (s *PDFGeneratorService) GetStats() ServiceStats {
	s.statsMutex.RLock()
	defer s.statsMutex.RUnlock()

	stats := s.stats

	// Count active jobs
	activeCount := 0
	s.activeJobs.Range(func(key, value interface{}) bool {
		activeCount++
		return true
	})
	stats.ActiveJobs = activeCount
	stats.QueuedJobs = len(s.jobQueue)

	return stats
}

// HTTP Handlers
type HTTPHandler struct {
	service *PDFGeneratorService
}

// Generate single PDF
func (h *HTTPHandler) GeneratePDF(c *gin.Context) {
	// Rate limiting
	if !h.service.rateLimiter.Allow() {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Rate limit exceeded",
			"code":  "RATE_LIMIT_EXCEEDED",
		})
		return
	}

	var req PDFRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid request format",
			"code":  "INVALID_REQUEST",
		})
		return
	}

	// Validate HTML size
	if int64(len(req.HTML)) > h.service.config.MaxFileSize {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "HTML content exceeds maximum size",
			"code":  "CONTENT_TOO_LARGE",
		})
		return
	}

	// Apply default options
	options := h.applyDefaultOptions(req.Options)

	// Create job
	job := &PDFJob{
		ID:        primitive.NewObjectID(),
		HTML:      req.HTML,
		Options:   options,
		Status:    "pending",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		CacheKey:  h.service.generateCacheKey(req.HTML, options),
	}

	// Save job to MongoDB
	_, err := h.service.jobsCollection.InsertOne(h.service.ctx, job)
	if err != nil {
		h.service.logger.Error("Failed to save job", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to create job",
			"code":  "JOB_CREATION_FAILED",
		})
		return
	}

	// Add to processing queue
	select {
	case h.service.jobQueue <- job:
		// Job queued successfully
	default:
		// Queue is full
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Service temporarily unavailable",
			"code":  "SERVICE_BUSY",
		})
		return
	}

	// For synchronous response, wait for completion
	if c.Query("async") != "true" {
		h.waitForJobCompletion(c, job.ID)
		return
	}

	// Return job ID for asynchronous processing
	c.JSON(http.StatusAccepted, gin.H{
		"job_id": job.ID.Hex(),
		"status": "pending",
	})
}

// Wait for job completion and return PDF
func (h *HTTPHandler) waitForJobCompletion(c *gin.Context, jobID primitive.ObjectID) {
	timeout := time.After(h.service.config.JobTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			c.JSON(http.StatusRequestTimeout, gin.H{
				"error": "Job processing timeout",
				"code":  "PROCESSING_TIMEOUT",
			})
			return

		case <-ticker.C:
			var job PDFJob
			err := h.service.jobsCollection.FindOne(h.service.ctx, bson.M{"_id": jobID}).Decode(&job)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "Failed to retrieve job",
					"code":  "JOB_RETRIEVAL_FAILED",
				})
				return
			}

			switch job.Status {
			case "completed":
				c.Header("Content-Type", "application/pdf")
				c.Header("Content-Disposition", "attachment; filename=\"document.pdf\"")
				c.Data(http.StatusOK, "application/pdf", job.Result)
				return

			case "failed":
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": job.Error,
					"code":  "PDF_GENERATION_FAILED",
				})
				return
			}
		}
	}
}

// Get job status
func (h *HTTPHandler) GetJobStatus(c *gin.Context) {
	jobIDStr := c.Param("id")
	jobID, err := primitive.ObjectIDFromHex(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid job ID",
			"code":  "INVALID_JOB_ID",
		})
		return
	}

	var job PDFJob
	err = h.service.jobsCollection.FindOne(h.service.ctx, bson.M{"_id": jobID}).Decode(&job)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Job not found",
			"code":  "JOB_NOT_FOUND",
		})
		return
	}

	response := gin.H{
		"job_id":     job.ID.Hex(),
		"status":     job.Status,
		"created_at": job.CreatedAt,
		"updated_at": job.UpdatedAt,
	}

	if job.CompletedAt != nil {
		response["completed_at"] = *job.CompletedAt
	}

	if job.Error != "" {
		response["error"] = job.Error
	}

	c.JSON(http.StatusOK, response)
}

// Download completed PDF
func (h *HTTPHandler) DownloadPDF(c *gin.Context) {
	jobIDStr := c.Param("id")
	jobID, err := primitive.ObjectIDFromHex(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid job ID",
			"code":  "INVALID_JOB_ID",
		})
		return
	}

	var job PDFJob
	err = h.service.jobsCollection.FindOne(h.service.ctx, bson.M{"_id": jobID}).Decode(&job)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Job not found",
			"code":  "JOB_NOT_FOUND",
		})
		return
	}

	if job.Status != "completed" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "Job not completed",
			"code":   "JOB_NOT_COMPLETED",
			"status": job.Status,
		})
		return
	}

	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"document_%s.pdf\"", job.ID.Hex()))
	c.Data(http.StatusOK, "application/pdf", job.Result)
}

// Batch PDF generation
func (h *HTTPHandler) GenerateBatchPDF(c *gin.Context) {
	var req BatchPDFRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid request format",
			"code":  "INVALID_REQUEST",
		})
		return
	}

	if len(req.Documents) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "No documents provided",
			"code":  "NO_DOCUMENTS",
		})
		return
	}

	if len(req.Documents) > 10 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Maximum 10 documents allowed per batch",
			"code":  "BATCH_SIZE_EXCEEDED",
		})
		return
	}

	var jobIDs []string
	results := make([]gin.H, len(req.Documents))

	for i, doc := range req.Documents {
		// Merge global and document options
		options := h.mergeOptions(req.Options, doc.Options)

		job := &PDFJob{
			ID:        primitive.NewObjectID(),
			HTML:      doc.HTML,
			Options:   options,
			Status:    "pending",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			CacheKey:  h.service.generateCacheKey(doc.HTML, options),
		}

		// Save job
		_, err := h.service.jobsCollection.InsertOne(h.service.ctx, job)
		if err != nil {
			results[i] = gin.H{
				"index":   i,
				"success": false,
				"error":   "Failed to create job",
			}
			continue
		}

		jobIDs = append(jobIDs, job.ID.Hex())

		// Queue job
		select {
		case h.service.jobQueue <- job:
			results[i] = gin.H{
				"index":  i,
				"job_id": job.ID.Hex(),
				"status": "pending",
			}
		default:
			results[i] = gin.H{
				"index":   i,
				"success": false,
				"error":   "Service busy",
			}
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"batch_id": primitive.NewObjectID().Hex(),
		"results":  results,
	})
}

// Health check
func (h *HTTPHandler) HealthCheck(c *gin.Context) {
	stats := h.service.GetStats()

	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now(),
		"stats":     stats,
		"version":   "1.0.0",
	})
}

// Apply default PDF options
func (h *HTTPHandler) applyDefaultOptions(options PDFOptions) PDFOptions {
	if options.Format == "" {
		options.Format = "A4"
	}
	if options.Orientation == "" {
		options.Orientation = "portrait"
	}
	if options.Margin == nil {
		options.Margin = map[string]string{
			"top":    "1cm",
			"bottom": "1cm",
			"left":   "1cm",
			"right":  "1cm",
		}
	}
	if options.Scale == 0 {
		options.Scale = 1.0
	}
	return options
}

// Merge PDF options
func (h *HTTPHandler) mergeOptions(global, doc PDFOptions) PDFOptions {
	merged := global

	if doc.Format != "" {
		merged.Format = doc.Format
	}
	if doc.Orientation != "" {
		merged.Orientation = doc.Orientation
	}
	if doc.Margin != nil {
		merged.Margin = doc.Margin
	}
	if doc.HeaderTemplate != "" {
		merged.HeaderTemplate = doc.HeaderTemplate
	}
	if doc.FooterTemplate != "" {
		merged.FooterTemplate = doc.FooterTemplate
	}
	if doc.Scale != 0 {
		merged.Scale = doc.Scale
	}

	merged.PrintBackground = doc.PrintBackground || global.PrintBackground
	merged.DisplayHeaderFooter = doc.DisplayHeaderFooter || global.DisplayHeaderFooter
	merged.PreferCSSPageSize = doc.PreferCSSPageSize || global.PreferCSSPageSize

	return h.applyDefaultOptions(merged)
}

// Setup HTTP routes
func (h *HTTPHandler) setupRoutes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	// CORS
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"*"},
		ExposeHeaders:    []string{"Content-Length", "Content-Disposition"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Routes
	v1 := r.Group("/api/v1")
	{
		v1.POST("/pdf/generate", h.GeneratePDF)
		v1.POST("/pdf/batch", h.GenerateBatchPDF)
		v1.GET("/pdf/job/:id", h.GetJobStatus)
		v1.GET("/pdf/download/:id", h.DownloadPDF)
		v1.GET("/health", h.HealthCheck)
	}

	return r
}

// Cleanup function
func (s *PDFGeneratorService) Cleanup() {
	s.logger.Info("Shutting down PDF Generator Service")

	s.cancel()

	if s.mongoClient != nil {
		s.mongoClient.Disconnect(context.Background())
	}

	if s.redisClient != nil {
		s.redisClient.Close()
	}

	s.logger.Sync()
}

// Load configuration from environment
func loadConfig() *Config {
	config := &Config{
		Port:              getEnv("PORT", "8080"),
		MaxConcurrentJobs: getEnvAsInt("MAX_CONCURRENT_JOBS", 5),
		JobTimeout:        time.Duration(getEnvAsInt("JOB_TIMEOUT_MS", 60000)) * time.Millisecond,
		CacheEnabled:      getEnvAsBool("CACHE_ENABLED", true),
		CacheTTL:          time.Duration(getEnvAsInt("CACHE_TTL_SECONDS", 3600)) * time.Second,
		MaxFileSize:       int64(getEnvAsInt("MAX_FILE_SIZE", 50*1024*1024)), // 50MB
		MongoURI:          getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:           getEnv("MONGO_DB", "pdf_service"),
		RedisAddr:         getEnv("REDIS_ADDR", "localhost:6379"),
		ChromeExecutable:  getEnv("CHROME_EXECUTABLE", "/usr/bin/google-chrome"),
		TempDir:           getEnv("TEMP_DIR", "/tmp"),
		RateLimitRequests: getEnvAsInt("RATE_LIMIT_REQUESTS", 100),
		RateLimitWindow:   time.Duration(getEnvAsInt("RATE_LIMIT_WINDOW_SECONDS", 900)) * time.Second,
	}

	return config
}

// Environment helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(name string, defaultVal int) int {
	valueStr := getEnv(name, "")
	if value, err := strconv.Atoi(valueStr); err == nil {
		return value
	}
	return defaultVal
}

func getEnvAsBool(name string, defaultVal bool) bool {
	valueStr := getEnv(name, "")
	if value, err := strconv.ParseBool(valueStr); err == nil {
		return value
	}
	return defaultVal
}

// Main function
func main() {
	config := loadConfig()

	// Initialize service
	service, err := NewPDFGeneratorService(config)
	if err != nil {
		log.Fatalf("Failed to initialize PDF service: %v", err)
	}

	// Setup HTTP handler
	handler := &HTTPHandler{service: service}
	router := handler.setupRoutes()

	// Start HTTP server
	server := &http.Server{
		Addr:    ":" + config.Port,
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		service.logger.Info("Shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			service.logger.Error("Server shutdown error", zap.Error(err))
		}

		service.Cleanup()
	}()

	service.logger.Info("Starting PDF Generator Service",
		zap.String("port", config.Port),
		zap.Int("max_concurrent_jobs", config.MaxConcurrentJobs))

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed to start: %v", err)
	}
}
