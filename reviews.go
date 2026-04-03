package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
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
	"sort"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"cloud.google.com/go/storage"
	"github.com/xuri/excelize/v2"
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

	// Fetch version history
	var allVersions []VersionRelease

	fmt.Print("Fetching iOS version history... ")
	iosVersions, err := fetchAppleVersionHistory()
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	} else {
		fmt.Printf("✓ %d version(s)\n", len(iosVersions))
		allVersions = append(allVersions, iosVersions...)
	}

	androidVersions := deriveAndroidVersionHistory(allReviews)
	fmt.Printf("Derived %d Android version(s) from reviews\n", len(androidVersions))
	allVersions = append(allVersions, androidVersions...)

	fmt.Println()

	// Export to XLSX
	if len(allReviews) > 0 {
		exeDir := "."
		if exePath, err := os.Executable(); err == nil {
			exeDir = filepath.Dir(exePath)
		}
		exportDir := filepath.Join(exeDir, "reviews_export")
		os.MkdirAll(exportDir, 0755)

		today := time.Now().Format("2006-01-02")
		xlsxFile := filepath.Join(exportDir, fmt.Sprintf("reviews_%s_to_%s.xlsx", dateInput, today))

		if err := exportXLSX(xlsxFile, allReviews, allVersions); err != nil {
			fmt.Fprintf(os.Stderr, "✗ Error writing XLSX: %v\n", err)
		} else {
			fmt.Printf("Exported %d review(s) to %s\n", len(allReviews), xlsxFile)
			openFile(xlsxFile)
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
	Version  string
}

type VersionRelease struct {
	Platform    string
	Version     string
	ReleaseDate string
}

// ============================================================
// Apple App Store Connect API
// ============================================================

func fetchAppleReviews(since time.Time) ([]Review, error) {
	token, err := createAppleJWT()
	if err != nil {
		return nil, fmt.Errorf("creating JWT: %w", err)
	}

	// Fetch all app store versions to get version strings per review
	type versionInfo struct {
		id      string
		version string
	}
	var versions []versionInfo
	versionsURL := fmt.Sprintf(
		"https://api.appstoreconnect.apple.com/v1/apps/%s/appStoreVersions?fields[appStoreVersions]=versionString&limit=200",
		appleAppID,
	)
	for versionsURL != "" {
		req, _ := http.NewRequest("GET", versionsURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching app store versions: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("appStoreVersions API returned %d: %s", resp.StatusCode, truncate(string(body), 300))
		}
		var vResult struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					VersionString string `json:"versionString"`
				} `json:"attributes"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := json.Unmarshal(body, &vResult); err != nil {
			return nil, fmt.Errorf("parsing versions response: %w", err)
		}
		for _, v := range vResult.Data {
			versions = append(versions, versionInfo{id: v.ID, version: v.Attributes.VersionString})
		}
		versionsURL = vResult.Links.Next
	}

	// Fetch reviews per version
	var allReviews []Review
	for _, v := range versions {
		nextURL := fmt.Sprintf(
			"https://api.appstoreconnect.apple.com/v1/appStoreVersions/%s/customerReviews?sort=-createdDate&limit=100",
			v.id,
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
					Version:  v.version,
				})
			}

			nextURL = result.Links.Next
		}
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
	if errors.Is(err, storage.ErrObjectNotExist) {
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

		version := ""
		if idx, ok := col["App Version Name"]; ok && idx < len(record) {
			version = strings.TrimSpace(record[idx])
		}

		reviews = append(reviews, Review{
			Platform: "Android",
			Author:   "", // GCS exports don't include author names
			Title:    title,
			Body:     body,
			Rating:   rating,
			Date:     reviewTime.Format("2006-01-02 15:04"),
			Version:  version,
		})
	}

	return reviews, nil
}

// ============================================================
// Apple Version History
// ============================================================

func fetchAppleVersionHistory() ([]VersionRelease, error) {
	token, err := createAppleJWT()
	if err != nil {
		return nil, fmt.Errorf("creating JWT: %w", err)
	}

	var versions []VersionRelease
	nextURL := fmt.Sprintf(
		"https://api.appstoreconnect.apple.com/v1/apps/%s/appStoreVersions?fields[appStoreVersions]=versionString,createdDate,appStoreState&limit=100",
		appleAppID,
	)

	for nextURL != "" {
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
					VersionString string `json:"versionString"`
					CreatedDate   string `json:"createdDate"`
					AppStoreState string `json:"appStoreState"`
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
			if d.Attributes.AppStoreState != "READY_FOR_DISTRIBUTION" &&
				d.Attributes.AppStoreState != "READY_FOR_SALE" {
				continue
			}
			releaseDate := d.Attributes.CreatedDate
			if len(releaseDate) >= 10 {
				releaseDate = releaseDate[:10]
			}
			versions = append(versions, VersionRelease{
				Platform:    "iOS",
				Version:     d.Attributes.VersionString,
				ReleaseDate: releaseDate,
			})
		}

		nextURL = result.Links.Next
	}

	return versions, nil
}

// ============================================================
// Android Version History (derived from review data)
// ============================================================

func deriveAndroidVersionHistory(reviews []Review) []VersionRelease {
	earliest := make(map[string]string) // version -> earliest date
	for _, r := range reviews {
		if r.Platform != "Android" || r.Version == "" {
			continue
		}
		date := r.Date
		if len(date) > 10 {
			date = date[:10] // truncate time part
		}
		if existing, ok := earliest[r.Version]; !ok || date < existing {
			earliest[r.Version] = date
		}
	}

	var versions []VersionRelease
	for v, d := range earliest {
		versions = append(versions, VersionRelease{
			Platform:    "Android",
			Version:     v,
			ReleaseDate: d,
		})
	}
	return versions
}

// ============================================================
// XLSX Export
// ============================================================

func exportXLSX(filename string, reviews []Review, versions []VersionRelease) error {
	f := excelize.NewFile()
	defer f.Close()

	// Bold header style
	boldStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
	})

	// --- Sheet 1: Reviews ---
	sheet1 := "Reviews"
	f.SetSheetName("Sheet1", sheet1)

	reviewHeaders := []string{"Platform", "Date", "Rating", "Author", "Title", "Version", "Review"}
	for i, h := range reviewHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet1, cell, h)
		f.SetCellStyle(sheet1, cell, cell, boldStyle)
	}

	for row, r := range reviews {
		rowNum := row + 2
		f.SetCellValue(sheet1, cellName(1, rowNum), r.Platform)
		f.SetCellValue(sheet1, cellName(2, rowNum), r.Date)
		f.SetCellValue(sheet1, cellName(3, rowNum), r.Rating)
		f.SetCellValue(sheet1, cellName(4, rowNum), r.Author)
		f.SetCellValue(sheet1, cellName(5, rowNum), r.Title)
		f.SetCellValue(sheet1, cellName(6, rowNum), r.Version)
		f.SetCellValue(sheet1, cellName(7, rowNum), r.Body)
	}

	// --- Sheet 2: Version History ---
	sheet2 := "Version History"
	f.NewSheet(sheet2)

	// Sort versions: by platform then release date descending
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Platform != versions[j].Platform {
			return versions[i].Platform < versions[j].Platform
		}
		return versions[i].ReleaseDate > versions[j].ReleaseDate
	})

	versionHeaders := []string{"Platform", "Version", "Release Date"}
	for i, h := range versionHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet2, cell, h)
		f.SetCellStyle(sheet2, cell, cell, boldStyle)
	}

	for row, v := range versions {
		rowNum := row + 2
		f.SetCellValue(sheet2, cellName(1, rowNum), v.Platform)
		f.SetCellValue(sheet2, cellName(2, rowNum), v.Version)
		f.SetCellValue(sheet2, cellName(3, rowNum), v.ReleaseDate)
	}

	return f.SaveAs(filename)
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
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
