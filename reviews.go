package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"cloud.google.com/go/storage"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"google.golang.org/api/option"
)

// ============================================================
// CONFIGURATION — Fill these in before compiling
// ============================================================

// Apple App Store Connect
const appleKeyID = "D24CF5M54Z"
const appleIssuerID = "69a6de75-28e6-47e3-e053-5b8c7c11a4d1"
const appleAppID = "1068461104"

// Google Play
const googlePackageName = "com.dayuse_hotels.dayuseus"
const gcsBucket = "pubsite_prod_rev_04666068877155168208"
const gcsPrefix = "reviews/reviews_" + googlePackageName + "_"

// ============================================================
// EMBEDDED CREDENTIALS — place files next to this .go file
// before running `go build`
//
//   AuthKey.p8 — your Apple private key
// ============================================================

//go:embed AuthKey.p8
var applePrivateKey []byte

//go:embed google-service-account.json
var googleServiceAccount []byte

// ============================================================

func main() {
	fmt.Println("=== App Reviews Fetcher ===")
	fmt.Println()

	defaultDate := time.Now().AddDate(0, 0, -31).Format("2006-01-02")
	fmt.Printf("Fetch reviews since (YYYY-MM-DD) [%s]: ", defaultDate)

	var dateInput string
	fmt.Scanln(&dateInput)
	if dateInput == "" {
		dateInput = defaultDate
	}

	sinceDate, err := time.Parse("2006-01-02", dateInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid date format. Use YYYY-MM-DD.\n")
		os.Exit(1)
	}

	fmt.Println()

	var allReviews []Review

	// Fetch iOS reviews
	fmt.Print("Fetching iOS reviews... ")
	iosReviews, err := fetchAppleReviews(sinceDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	} else {
		fmt.Printf("✓ %d review(s)\n", len(iosReviews))
		allReviews = append(allReviews, iosReviews...)
	}

	// Fetch Android reviews
	fmt.Print("Fetching Android reviews... ")
	androidReviews, err := fetchGoogleReviews(sinceDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	} else {
		fmt.Printf("✓ %d review(s)\n", len(androidReviews))
		allReviews = append(allReviews, androidReviews...)
	}

	fmt.Println()

	// Export to CSV
	if len(allReviews) > 0 {
		exeDir := "."
		if exePath, err := os.Executable(); err == nil {
			exeDir = filepath.Dir(exePath)
		}
		exportDir := filepath.Join(exeDir, "reviews_export")
		os.MkdirAll(exportDir, 0755)

		today := time.Now().Format("2006-01-02")
		csvFile := filepath.Join(exportDir, fmt.Sprintf("reviews_%s_to_%s.csv", dateInput, today))

		if err := exportCSV(csvFile, allReviews); err != nil {
			fmt.Fprintf(os.Stderr, "✗ Error writing CSV: %v\n", err)
		} else {
			fmt.Printf("Exported %d review(s) to %s\n", len(allReviews), csvFile)
			openFile(csvFile)
		}
	} else {
		fmt.Println("No reviews found since", dateInput)
	}

	fmt.Println("Done.")
}

// ============================================================
// Common
// ============================================================

var errNotFound = fmt.Errorf("not found")

type Review struct {
	Platform string
	Author   string
	Title    string
	Body     string
	Rating   int
	Date     string
}

// ============================================================
// Apple App Store Connect API
// ============================================================

func fetchAppleReviews(since time.Time) ([]Review, error) {
	token, err := createAppleJWT()
	if err != nil {
		return nil, fmt.Errorf("creating JWT: %w", err)
	}

	var allReviews []Review
	// Sort newest first; no server-side date filter available, so we paginate
	// and stop once reviews are older than the requested date.
	nextURL := fmt.Sprintf(
		"https://api.appstoreconnect.apple.com/v1/apps/%s/customerReviews?sort=-createdDate&limit=100",
		appleAppID,
	)

	done := false
	for nextURL != "" && !done {
		req, err := http.NewRequest("GET", nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(body), 300))
		}

		var result struct {
			Data []struct {
				Attributes struct {
					Rating           int    `json:"rating"`
					Title            string `json:"title"`
					Body             string `json:"body"`
					ReviewerNickname string `json:"reviewerNickname"`
					CreatedDate      string `json:"createdDate"`
				} `json:"attributes"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		for _, d := range result.Data {
			t, err := time.Parse(time.RFC3339, d.Attributes.CreatedDate)
			if err == nil && t.Before(since) {
				done = true
				break
			}
			allReviews = append(allReviews, Review{
				Platform: "iOS",
				Author:   d.Attributes.ReviewerNickname,
				Title:    d.Attributes.Title,
				Body:     d.Attributes.Body,
				Rating:   d.Attributes.Rating,
				Date:     d.Attributes.CreatedDate[:10],
			})
		}

		nextURL = result.Links.Next
	}

	return allReviews, nil
}

func createAppleJWT() (string, error) {
	block, _ := pem.Decode(applePrivateKey)
	if block == nil {
		return "", fmt.Errorf("failed to decode .p8 PEM")
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing private key: %w", err)
	}

	ecKey, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("key is not ECDSA")
	}

	now := time.Now()
	header := map[string]string{
		"alg": "ES256",
		"kid": appleKeyID,
		"typ": "JWT",
	}
	payload := map[string]interface{}{
		"iss": appleIssuerID,
		"iat": now.Unix(),
		"exp": now.Add(20 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1",
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	signingInput := b64url(headerJSON) + "." + b64url(payloadJSON)

	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, ecKey, hash[:])
	if err != nil {
		return "", fmt.Errorf("signing: %w", err)
	}

	// IEEE P1363: r || s, each zero-padded to 32 bytes
	curveBits := ecKey.Curve.Params().BitSize
	keyBytes := (curveBits + 7) / 8
	sig := make([]byte, 2*keyBytes)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(sig[keyBytes-len(rBytes):keyBytes], rBytes)
	copy(sig[2*keyBytes-len(sBytes):], sBytes)

	return signingInput + "." + b64url(sig), nil
}

// ============================================================
// Google Play Reviews via GCS (fetched at runtime)
// ============================================================

func fetchGoogleReviews(since time.Time) ([]Review, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithCredentialsJSON(googleServiceAccount))
	if err != nil {
		return nil, fmt.Errorf("creating GCS client: %w", err)
	}
	defer client.Close()

	bucket := client.Bucket(gcsBucket)

	// Determine which monthly CSV files we need (from since month to current month)
	now := time.Now()
	var months []string
	cursor := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !cursor.After(end) {
		months = append(months, cursor.Format("200601"))
		cursor = cursor.AddDate(0, 1, 0)
	}

	var allReviews []Review
	for _, month := range months {
		objectName := gcsPrefix + month + ".csv"
		reviews, err := fetchAndParseGCSCSV(ctx, bucket, objectName, since)
		if err == errNotFound {
			continue // no data for this month — expected
		}
		if err != nil {
			return nil, fmt.Errorf("month %s: %w", month, err)
		}
		allReviews = append(allReviews, reviews...)
	}

	return allReviews, nil
}

func fetchAndParseGCSCSV(ctx context.Context, bucket *storage.BucketHandle, objectName string, since time.Time) ([]Review, error) {
	// Check if the object exists first
	obj := bucket.Object(objectName)
	_, err := obj.Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return nil, errNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checking object %s: %w", objectName, err)
	}

	rc, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading object %s: %w", objectName, err)
	}
	defer rc.Close()

	return parseGCSReviewCSV(rc, since)
}

func parseGCSReviewCSV(reader io.Reader, since time.Time) ([]Review, error) {
	// Google exports CSVs in UTF-16LE with BOM — decode to UTF-8
	utf16Reader := transform.NewReader(reader, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder())

	r := csv.NewReader(utf16Reader)
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}

	// Build column index map so we don't rely on fixed positions
	col := make(map[string]int)
	for i, name := range header {
		col[name] = i
	}

	// Required columns
	needed := []string{"Review Submit Millis Since Epoch", "Star Rating", "Review Text"}
	for _, n := range needed {
		if _, ok := col[n]; !ok {
			return nil, fmt.Errorf("missing expected column %q in CSV", n)
		}
	}

	var reviews []Review
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed rows
		}

		millis, _ := strconv.ParseInt(record[col["Review Submit Millis Since Epoch"]], 10, 64)
		reviewTime := time.UnixMilli(millis)
		if reviewTime.Before(since) {
			continue
		}

		body := strings.TrimSpace(record[col["Review Text"]])
		if body == "" {
			continue // skip reviews with no comment
		}

		rating, _ := strconv.Atoi(record[col["Star Rating"]])

		title := ""
		if idx, ok := col["Review Title"]; ok && idx < len(record) {
			title = strings.TrimSpace(record[idx])
		}

		reviews = append(reviews, Review{
			Platform: "Android",
			Author:   "", // GCS exports don't include author names
			Title:    title,
			Body:     body,
			Rating:   rating,
			Date:     reviewTime.Format("2006-01-02 15:04"),
		})
	}

	return reviews, nil
}

// ============================================================
// CSV Export
// ============================================================

func exportCSV(filename string, reviews []Review) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write BOM for Excel compatibility
	f.Write([]byte{0xEF, 0xBB, 0xBF})

	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"Platform", "Date", "Rating", "Author", "Title", "Review"})
	for _, r := range reviews {
		w.Write([]string{
			r.Platform,
			r.Date,
			fmt.Sprintf("%d", r.Rating),
			r.Author,
			r.Title,
			r.Body,
		})
	}

	return w.Error()
}

// ============================================================
// Helpers
// ============================================================

func b64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func openFile(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	cmd.Start()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
