package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "embed"
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

// ============================================================
// EMBEDDED CREDENTIALS — place files next to this .go file
// before running `go build`
//
//   AuthKey.p8                — your Apple private key
//   google-service-account.json — your Google service account key
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
	fmt.Println("── iOS App Store Reviews ──────────────────────")
	iosReviews, err := fetchAppleReviews(sinceDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
	} else if len(iosReviews) == 0 {
		fmt.Println("  No reviews found since", dateInput)
	} else {
		fmt.Printf("  Found %d review(s)\n", len(iosReviews))
		for _, r := range iosReviews {
			printReview(r)
		}
		allReviews = append(allReviews, iosReviews...)
	}

	fmt.Println()

	// Fetch Android reviews
	fmt.Println("── Google Play Reviews ────────────────────────")
	androidReviews, err := fetchGoogleReviews(sinceDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
	} else if len(androidReviews) == 0 {
		fmt.Println("  No reviews found since", dateInput)
	} else {
		fmt.Printf("  Found %d review(s)\n", len(androidReviews))
		for _, r := range androidReviews {
			printReview(r)
		}
		allReviews = append(allReviews, androidReviews...)
	}

	fmt.Println()

	// Export to CSV
	if len(allReviews) > 0 {
		// Create reviews_export directory next to the binary
		exeDir := "."
		if exePath, err := os.Executable(); err == nil {
			exeDir = filepath.Dir(exePath)
		}
		exportDir := filepath.Join(exeDir, "reviews_export")
		os.MkdirAll(exportDir, 0755)

		today := time.Now().Format("2006-01-02")
		csvFile := filepath.Join(exportDir, fmt.Sprintf("reviews_%s_to_%s.csv", dateInput, today))

		if err := exportCSV(csvFile, allReviews); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing CSV: %v\n", err)
		} else {
			fmt.Printf("Exported %d review(s) to %s\n", len(allReviews), csvFile)
			openFile(csvFile)
		}
	}

	fmt.Println("Done.")
}

// ============================================================
// Common
// ============================================================

type Review struct {
	Platform string
	Author   string
	Title    string
	Body     string
	Rating   int
	Date     string
}

func printReview(r Review) {
	stars := strings.Repeat("★", r.Rating) + strings.Repeat("☆", 5-r.Rating)
	fmt.Printf("\n  %s  %s  —  %s\n", stars, r.Date, r.Author)
	if r.Title != "" {
		fmt.Printf("  %s\n", r.Title)
	}
	fmt.Printf("  %s\n", r.Body)
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
// Google Play Developer API
// ============================================================

func fetchGoogleReviews(since time.Time) ([]Review, error) {
	token, err := createGoogleAccessToken()
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}

	var allReviews []Review
	nextToken := ""

	for {
		u := fmt.Sprintf(
			"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/reviews?maxResults=100",
			googlePackageName,
		)
		if nextToken != "" {
			u += "&token=" + url.QueryEscape(nextToken)
		}

		req, err := http.NewRequest("GET", u, nil)
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
			Reviews []struct {
				AuthorName string `json:"authorName"`
				Comments   []struct {
					UserComment struct {
						Text         string `json:"text"`
						StarRating   int    `json:"starRating"`
						LastModified struct {
							Seconds string `json:"seconds"`
						} `json:"lastModified"`
					} `json:"userComment"`
				} `json:"comments"`
			} `json:"reviews"`
			TokenPagination struct {
				NextPageToken string `json:"nextPageToken"`
			} `json:"tokenPagination"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		for _, rv := range result.Reviews {
			if len(rv.Comments) == 0 {
				continue
			}
			uc := rv.Comments[0].UserComment

			var secs int64
			fmt.Sscanf(uc.LastModified.Seconds, "%d", &secs)
			reviewTime := time.Unix(secs, 0)

			if reviewTime.Before(since) {
				continue
			}

			allReviews = append(allReviews, Review{
				Platform: "Android",
				Author:   rv.AuthorName,
				Body:     uc.Text,
				Rating:   uc.StarRating,
				Date:     reviewTime.Format("2006-01-02 15:04"),
			})
		}

		nextToken = result.TokenPagination.NextPageToken
		if nextToken == "" {
			break
		}
	}

	return allReviews, nil
}

func createGoogleAccessToken() (string, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(googleServiceAccount, &sa); err != nil {
		return "", fmt.Errorf("parsing service account JSON: %w", err)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}

	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]interface{}{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/androidpublisher",
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}

	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signingInput := b64url(headerJSON) + "." + b64url(claimsJSON)

	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("failed to decode service account PEM key")
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing RSA key: %w", err)
	}

	rsaKey, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("key is not RSA")
	}

	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	jwt := signingInput + "." + b64url(sig)

	// Exchange JWT assertion for an access token
	resp, err := http.PostForm(sa.TokenURI, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token exchange failed %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", err
	}

	return tokenResp.AccessToken, nil
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
