# GridX Billing & Wallet Service

The **Billing & Wallet Service** is a Go microservice in the GridX platform responsible for owning all money-like state: managing user credit balances, reserving funds for open buy orders, executing atomic trade settlements, calculating platform fee commissions, and integrating with Stripe Checkout for user deposits and withdrawals. It is the platform's single source of financial truth.

---

# Repository Structure

```text
billing-service/
├── cmd/
│   ├── billing/                  # Application entrypoint: parse config, wire dependencies, handle signals
│   └── migrate/                  # Goose database migration utility
├── internal/
│   ├── app/                      # Application wiring, server lifecycle, graceful shutdown
│   ├── config/                   # Typed environment configuration and startup validation
│   ├── domain/                   # Domain models (Wallet, Hold, Journal), invariants, and sentinel errors
│   ├── service/                  # Core financial use-cases (Reserve, Settle, Wallet operations)
│   ├── store/
│   │   ├── postgres/             # PostgreSQL database pool, row locking, ledgers, outbox store
│   │   └── redis/                # Redis client adapter and RPC idempotency response caching
│   ├── provider/
│   │   └── stripe/               # Stripe Checkout and Webhook provider implementation
│   ├── transport/
│   │   └── grpc/                 # gRPC server setup, interceptors, and service handlers
│   ├── worker/
│   │   ├── outbox_relay.go       # Background transactional outbox worker
│   │   └── reconciler.go         # Background sweeper for expired holds & pending deposits
│   └── observability/            # Structured logging (slog), Prometheus metrics, OpenTelemetry
├── migrations/                   # Goose SQL migrations (embedded via Go embed)
├── test/integration/             # Integration tests using real PostgreSQL and Redis (Testcontainers)
├── docs/runbooks/                # Runbooks for financial reconciliation and failure scenarios
├── .env.example
├── Dockerfile                    # Multi-stage non-root container image
├── docker-compose.yml            # Local development compose integration
└── Taskfile.yml                  # Task runner commands
```

---

# Clone the Project

When working inside the GridX workspace:

```bash
cd gridx-workspace
```

The repository should exist at:

```text
gridx-workspace/billing-service
```

or you can clone it by running

```bash
go-task setup
```

or

```bash
task setup
```

---

# Install Go Dependencies

From the `billing-service` directory:

```bash
go mod download
```

Then verify and normalize the module dependencies:

```bash
go mod tidy
```

---

# Local Infrastructure

The Billing Service depends on PostgreSQL and Redis provided by `gridx-infra`.

From the parent workspace:

```bash
cd gridx-workspace
```

Start the GridX environment:

```bash
go-task up
# Or
task up
```

---

# Configuration and Operations

All settings are configured via environment variables (a local `.env` file is loaded for local development; see `.env.example`). **A missing required configuration variable or invalid format fails startup immediately.**

---

# Money and Accounting Invariants

To guarantee financial integrity across operations:

1. **Integer Minor Units:** All monetary values are represented as minor units using 64-bit signed integers (`BIGINT`). $1 \text{ credit} = 100 \text{ minor units}$. No floating-point math is allowed.
2. **Double-Entry Ledger:** Every `journal` movement contains multiple `ledger_lines` whose amounts strictly sum to **0** (`SUM(amount) = 0`).
3. **Database Guardrails:** Wallets strictly enforce positive balances (`available >= 0`, `reserved >= 0`) via Postgres `CHECK` constraints.
4. **Row-Level Locking:** All operations affecting wallet balances run under explicit Postgres `SELECT ... FOR UPDATE` locks, sorted deterministically by `wallet_id` to prevent deadlocks.

---

# Database Migrations

Database migrations are managed using **Goose**.

---

## Migration Files

Migration SQL files are stored in:

Example:

```text
migrations/
├── embed.go
├── <TIMESTAMP>_initial_schema.sql
├── <TIMESTAMP>_fee_configs.sql
└── ...
```

The migration SQL files are embedded into the Go executable via `migrations/embed.go`.

---

## Migration Commands

Ensure PostgreSQL is running, then configure `POSTGRES_URL`.

### Apply Migrations Locally

```bash
go run ./cmd/migrate up
# Or using Taskfile:
task db:up
```

### Check Migration Status

```bash
go run ./cmd/migrate status
# Or using Taskfile:
task db:status
```

### Check Migration Version

```bash
go run ./cmd/migrate version
# Or using Taskfile:
task db:version
```

### Roll Back the Latest Migration

```bash
go run ./cmd/migrate down
# Or using Taskfile:
task db:down
```

---

# Run the Application Locally

---

## Option 1: Infrastructure in Docker, Service on Host

### 1. Start Infrastructure

From `gridx-workspace`:

```bash
task infra-up
```

### 2. Move to Service Directory

```bash
cd billing-service
```

### 3. Apply Migrations

```bash
go run ./cmd/migrate up
```

### 4. Run the Service

```bash
go run ./cmd/billing
```

---

## Option 2: Full GridX Container Suite

From `gridx-workspace`:

```bash
task build -- billing-service
task up
```

---

# Running Tests

Run unit and property tests:

```bash
go test ./...
```

Run tests with verbose output:

```bash
go test -v ./...
```

Run tests with the Go race detector enabled:

```bash
go test -race ./...
```

Run integration tests if available (requires Docker for testcontainers PostgreSQL and Redis):

```bash
go test -tags integration ./test/integration/...
```

---

# Formatting and Static Checks

Format code:

```bash
gofmt -w .
```

Run compiler check:

```bash
go vet ./...
```

Run static analysis (if installed):

```bash
staticcheck ./...
```

Recommended verification sequence before pushing:

```bash
gofmt -w .
go vet ./...
go test -race ./...
```

---

# Build the Go Binaries

Build the billing service binary:

```bash
go build -o bin/billing ./cmd/billing
```

Build the migration binary:

```bash
go build -o bin/migrate ./cmd/migrate
```

The resulting binaries will be created in `bin/`:

```text
bin/
├── billing
└── migrate
```

Run the application binary:

```bash
./bin/billing
```