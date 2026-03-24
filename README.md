# App Reviews Fetcher

CLI tool that fetches app reviews from the **Apple App Store** and **Google Play Store**, displays them in the terminal, and exports them to CSV.

## Features

- Fetches iOS reviews via App Store Connect API
- Fetches Android reviews directly from Google Cloud Storage at runtime (full history, not limited to 7 days)
- Filters reviews by date (defaults to last 31 days)
- Exports all reviews to a CSV file (auto-opens on completion)

## Prerequisites

- Go 1.21+
- An Apple App Store Connect API key (`.p8` file)
- A Google Play service account JSON key with access to the GCS review exports bucket

## Setup

1. Place your Apple private key as `AuthKey.p8` in the project root
2. Place your Google service account key as `google-service-account.json` in the project root
3. Both files are embedded into the binary at build time — only needed when compiling, not when running
4. Update the configuration constants in `reviews.go`:
   - `appleKeyID` — your App Store Connect key ID
   - `appleIssuerID` — your issuer ID
   - `appleAppID` — your Apple app ID
   - `googlePackageName` — your Android package name

## Build & Run

```bash
go build -o reviews reviews.go

# Ad-hoc sign so macOS doesn't block the binary when shared
codesign --force --sign - reviews

./reviews
```

You'll be prompted for a start date (defaults to 31 days ago). Reviews are printed to the terminal and exported to `reviews_export/`.

> **Sharing the binary:** The ad-hoc codesign is sufficient for Apple Silicon Macs. If a recipient still sees a Gatekeeper warning, they can run `xattr -cr ./reviews` before executing it.

## Webhook Server (optional)

A simple Express server is included for testing webhook integrations:

```bash
npm install
node server.js
```

Listens on `http://localhost:3000/webhook` and logs all incoming requests.
