# Scenario: The health endpoint answers 200

## Metadata
- **ID**: HELLO-001
- **Layer**: HTTP Ingestion
- **Tags**: http, health, smoke

## TRIGGER
GET `${INGESTION_URL}/health`

## VERIFY
N/A — the HTTP Ingestion layer judges by status code.

## EXPECT

### Runnable
- status=200

## TIMEOUT
10s

## CLEANUP
N/A — a GET creates nothing.
