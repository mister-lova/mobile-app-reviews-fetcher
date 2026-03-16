# App Reviews Fetcher

CLI tool that fetches app reviews from the **Apple App Store** and **Google Play Store**, displays them in the terminal, and exports them to CSV.

## Features

- Fetches iOS reviews via App Store Connect API
- Fetches Android reviews via Google Play Developer API
- Filters reviews by date (defaults to last 31 days)
- Exports all reviews to a CSV file (auto-opens on completion)

## Prerequisites

- Go 1.21+
- An Apple App Store Connect API key (`.p8` file)
- A Google Play service account JSON key with Android Publisher API access

## Setup

1. Place your Apple private key as `AuthKey.p8` in the project root
2. Place your Google service account key as `google-service-account.json` in the project root

## Build & Run

```bash
go build -o reviews reviews.go
./reviews
```

You'll be prompted for a start date (defaults to 31 days ago). Reviews are printed to the terminal and exported to `reviews_export/`.

## Webhook Server (optional)

A simple Express server is included for testing webhook integrations:

```bash
npm install
node server.js
```

Listens on `http://localhost:3000/webhook` and logs all incoming requests.
