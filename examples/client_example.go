package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// PDFClient represents a client for the PDF generation service
type PDFClient struct {
	BaseURL    string
	HTTPClient *http.Client
	APIKey     string
}

// PDFOptions represents PDF generation options
type PDFOptions struct {
	Format              string            `json:"format,omitempty"`
	Orientation         string            `json:"orientation,omitempty"`
	Margin              map[string]string `json:"margin,omitempty"`
	PrintBackground     bool              `json:"printBackground,omitempty"`
	DisplayHeaderFooter bool              `json:"displayHeaderFooter,omitempty"`
	HeaderTemplate      string            `json:"headerTemplate,omitempty"`
	FooterTemplate      string            `json:"footerTemplate,omitempty"`
	Scale               float64           `json:"scale,omitempty"`
	PreferCSSPageSize   bool              `json:"preferCSSPageSize,omitempty"`
}

// PDFRequest represents a PDF generation request
type PDFRequest struct {
	HTML    string     `json:"html"`
	Options PDFOptions `json:"options"`
}

// BatchPDFRequest represents a batch PDF generation request
type BatchPDFRequest struct {
	Documents []PDFDocument `json:"documents"`
	Options   PDFOptions    `json:"options"`
}

type PDFDocument struct {
	HTML    string     `json:"html"`
	Options PDFOptions `json:"options"`
}

// JobResponse represents a job creation response
type JobResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// BatchResponse represents a batch job response
type BatchResponse struct {
	BatchID string `json:"batch_id"`
	Results []struct {
		Index   int    `json:"index"`
		JobID   string `json:"job_id,omitempty"`
		Success bool   `json:"success,omitempty"`
		Error   string `json:"error,omitempty"`
		Status  string `json:"status,omitempty"`
	} `json:"results"`
}

// JobStatus represents the status of a PDF generation job
type JobStatus struct {
	JobID       string     `json:"job_id"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Stats     struct {
		ActiveJobs     int     `json:"active_jobs"`
		QueuedJobs     int     `json:"queued_jobs"`
		CompletedJobs  int     `json:"completed_jobs"`
		FailedJobs     int     `json:"failed_jobs"`
		CacheHitRate   float64 `json:"cache_hit_rate"`
		AvgProcessTime float64 `json:"avg_process_time_ms"`
	} `json:"stats"`
	Version string `json:"version"`
}

// NewPDFClient creates a new PDF client
func NewPDFClient(baseURL string, apiKey ...string) *PDFClient {
	client := &PDFClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}

	if len(apiKey) > 0 {
		client.APIKey = apiKey[0]
	}

	return client
}

// makeRequest helper function for HTTP requests
func (c *PDFClient) makeRequest(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	return c.HTTPClient.Do(req)
}

// GeneratePDF generates a PDF synchronously
func (c *PDFClient) GeneratePDF(ctx context.Context, html string, options PDFOptions) ([]byte, error) {
	request := PDFRequest{
		HTML:    html,
		Options: options,
	}

	resp, err := c.makeRequest(ctx, "POST", "/api/v1/pdf/generate", request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return nil, fmt.Errorf("PDF generation failed: %v", errorResp["error"])
	}

	return io.ReadAll(resp.Body)
}

// GeneratePDFAsync generates a PDF asynchronously
func (c *PDFClient) GeneratePDFAsync(ctx context.Context, html string, options PDFOptions) (*JobResponse, error) {
	request := PDFRequest{
		HTML:    html,
		Options: options,
	}

	resp, err := c.makeRequest(ctx, "POST", "/api/v1/pdf/generate?async=true", request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return nil, fmt.Errorf("async PDF generation failed: %v", errorResp["error"])
	}

	var jobResp JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &jobResp, nil
}

// GenerateBatchPDF generates multiple PDFs in a batch
func (c *PDFClient) GenerateBatchPDF(ctx context.Context, documents []PDFDocument, globalOptions PDFOptions) (*BatchResponse, error) {
	request := BatchPDFRequest{
		Documents: documents,
		Options:   globalOptions,
	}

	resp, err := c.makeRequest(ctx, "POST", "/api/v1/pdf/batch", request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return nil, fmt.Errorf("batch PDF generation failed: %v", errorResp["error"])
	}

	var batchResp BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &batchResp, nil
}

// GetJobStatus retrieves the status of a PDF generation job
func (c *PDFClient) GetJobStatus(ctx context.Context, jobID string) (*JobStatus, error) {
	resp, err := c.makeRequest(ctx, "GET", "/api/v1/pdf/job/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return nil, fmt.Errorf("failed to get job status: %v", errorResp["error"])
	}

	var status JobStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &status, nil
}

// DownloadPDF downloads a completed PDF
func (c *PDFClient) DownloadPDF(ctx context.Context, jobID string) ([]byte, error) {
	resp, err := c.makeRequest(ctx, "GET", "/api/v1/pdf/download/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return nil, fmt.Errorf("failed to download PDF: %v", errorResp["error"])
	}

	return io.ReadAll(resp.Body)
}

// WaitForCompletion waits for a job to complete and returns the PDF
func (c *PDFClient) WaitForCompletion(ctx context.Context, jobID string, pollInterval time.Duration) ([]byte, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			status, err := c.GetJobStatus(ctx, jobID)
			if err != nil {
				return nil, err
			}

			switch status.Status {
			case "completed":
				return c.DownloadPDF(ctx, jobID)
			case "failed":
				return nil, fmt.Errorf("job failed: %s", status.Error)
			}
			// Continue polling for "pending" and "processing"
		}
	}
}

// CheckHealth checks the service health
func (c *PDFClient) CheckHealth(ctx context.Context) (*HealthResponse, error) {
	resp, err := c.makeRequest(ctx, "GET", "/api/v1/health", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var health HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to decode health response: %w", err)
	}

	return &health, nil
}

// Usage Examples

// Example 1: Basic PDF generation
func ExampleBasicPDFGeneration() {
	client := NewPDFClient("http://localhost:8080")
	ctx := context.Background()

	html := `
	<!DOCTYPE html>
	<html>
	<head>
		<title>Sample Document</title>
		<style>
			body { font-family: Arial, sans-serif; margin: 2cm; }
			h1 { color: #333; border-bottom: 2px solid #007bff; }
			.content { margin: 20px 0; text-align: justify; }
		</style>
	</head>
	<body>
		<h1>Chapter 1: Introduction</h1>
		<div class="content">
			<p>This is a sample PDF document generated from HTML content.</p>
			<p>The PDF generation service provides high-quality rendering of HTML with CSS support.</p>
		</div>
	</body>
	</html>`

	options := PDFOptions{
		Format:          "A4",
		Orientation:     "portrait",
		PrintBackground: true,
		Margin: map[string]string{
			"top":    "2cm",
			"bottom": "2cm",
			"left":   "2cm",
			"right":  "2cm",
		},
		DisplayHeaderFooter: true,
		HeaderTemplate:      `<div style="font-size: 10px; width: 100%; text-align: center;">Sample Document</div>`,
		FooterTemplate:      `<div style="font-size: 10px; width: 100%; text-align: center;">Page <span class="pageNumber"></span></div>`,
	}

	pdfData, err := client.GeneratePDF(ctx, html, options)
	if err != nil {
		fmt.Printf("Error generating PDF: %v\n", err)
		return
	}

	// Save to file
	if err := os.WriteFile("sample.pdf", pdfData, 0644); err != nil {
		fmt.Printf("Error saving PDF: %v\n", err)
		return
	}

	fmt.Println("PDF generated successfully: sample.pdf")
}

// Example 2: Asynchronous PDF generation with polling
func ExampleAsyncPDFGeneration() {
	client := NewPDFClient("http://localhost:8080")
	ctx := context.Background()

	html := `<html><body><h1>Async PDF Generation</h1><p>This PDF was generated asynchronously.</p></body></html>`
	options := PDFOptions{Format: "A4", PrintBackground: true}

	// Start async generation
	jobResp, err := client.GeneratePDFAsync(ctx, html, options)
	if err != nil {
		fmt.Printf("Error starting async PDF generation: %v\n", err)
		return
	}

	fmt.Printf("PDF generation started, job ID: %s\n", jobResp.JobID)

	// Wait for completion
	pdfData, err := client.WaitForCompletion(ctx, jobResp.JobID, 2*time.Second)
	if err != nil {
		fmt.Printf("Error waiting for completion: %v\n", err)
		return
	}

	// Save the PDF
	filename := fmt.Sprintf("async_%s.pdf", jobResp.JobID)
	if err := os.WriteFile(filename, pdfData, 0644); err != nil {
		fmt.Printf("Error saving PDF: %v\n", err)
		return
	}

	fmt.Printf("Async PDF generated successfully: %s\n", filename)
}

// Example 3: Batch PDF generation
func ExampleBatchPDFGeneration() {
	client := NewPDFClient("http://localhost:8080")
	ctx := context.Background()

	documents := []PDFDocument{
		{
			HTML: `<html><body><h1>Document 1</h1><p>First document content</p></body></html>`,
			Options: PDFOptions{
				HeaderTemplate: `<div>Document 1 Header</div>`,
			},
		},
		{
			HTML: `<html><body><h1>Document 2</h1><p>Second document content</p></body></html>`,
			Options: PDFOptions{
				HeaderTemplate: `<div>Document 2 Header</div>`,
			},
		},
		{
			HTML: `<html><body><h1>Document 3</h1><p>Third document content</p></body></html>`,
			Options: PDFOptions{
				HeaderTemplate: `<div>Document 3 Header</div>`,
			},
		},
	}

	globalOptions := PDFOptions{
		Format:              "A4",
		PrintBackground:     true,
		DisplayHeaderFooter: true,
		Margin: map[string]string{
			"top":    "2cm",
			"bottom": "1cm",
			"left":   "2cm",
			"right":  "2cm",
		},
	}

	batchResp, err := client.GenerateBatchPDF(ctx, documents, globalOptions)
	if err != nil {
		fmt.Printf("Error starting batch generation: %v\n", err)
		return
	}

	fmt.Printf("Batch processing started, batch ID: %s\n", batchResp.BatchID)

	// Process each result
	for _, result := range batchResp.Results {
		if result.JobID == "" {
			fmt.Printf("Document %d failed: %s\n", result.Index, result.Error)
			continue
		}

		fmt.Printf("Document %d job ID: %s\n", result.Index, result.JobID)

		// Wait for completion
		pdfData, err := client.WaitForCompletion(ctx, result.JobID, 2*time.Second)
		if err != nil {
			fmt.Printf("Document %d failed: %v\n", result.Index, err)
			continue
		}

		// Save the PDF
		filename := fmt.Sprintf("batch_doc_%d.pdf", result.Index)
		if err := os.WriteFile(filename, pdfData, 0644); err != nil {
			fmt.Printf("Error saving document %d: %v\n", result.Index, err)
			continue
		}

		fmt.Printf("Document %d saved as %s\n", result.Index, filename)
	}
}

func ExampleBookGeneration() {
	client := NewPDFClient("http://localhost:8080")
	ctx := context.Background()

	// Generate a complete book with table of contents
	bookHTML := `
	<!doctype html>
<html lang="en">`

}
