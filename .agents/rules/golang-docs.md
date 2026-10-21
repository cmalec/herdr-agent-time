---
description: Go documentation strategy - README, doc.go, runnable examples, context propagation, and GoDoc conventions
alwaysApply: false
globs:
  - "**/*.go"
  - "**/README.md"
  - "**/doc.go"
---
# Go Documentation Strategy

## Three-Tier Architecture

### Tier 1: README.md (Marketing + Quick Start)

- **Purpose:** Get users excited and productive quickly.
- **Include:** High-level problem/solution, key features, installation, prerequisites, and one simple "Happy Path" workflow.
- **Exclude:** Deep technical edge cases, method-by-method docs.

### Tier 2: doc.go Files (Package-Level Docs)

- **Purpose:** High-level architectural overview of a specific package.
- **Requirement:** Every package must have a `doc.go` file with a package-level comment.
- **Format:** Use Go 1.19+ heading structures (`# Heading`) and symbol links (e.g., `[Client]`).

### Tier 3: Runnable Examples (example_test.go)

- **Purpose:** Provide complex, compiler-verified code examples.
- **Convention:** Write examples in `example_test.go` (or `example_package_test.go`) using Go's standard `func Example()` naming convention. These are run during `go test` and render natively on pkg.go.dev.

---

## Context Propagation Guidelines

### Rules for Handling `context.Context`

- **First Parameter:** Any function/method performing network I/O, database queries, or blocking tasks must take `ctx context.Context` as its very first parameter.
- **No Context in Structs:** Never store a `context.Context` inside a struct (e.g., a `Client` struct). Always pass it explicitly through the call chain.
- **Respect Cancellation:** Ensure internal HTTP clients, gRPC calls, or channel operations actively listen to `ctx.Done()`.
- **Document Cancellation:** Explicitly document if a method supports cancellation or has an internal fallback timeout.

---

## Doc Comment Conventions & Style

### Exported Identifiers
All exported types, functions, constants, and variables must be documented.
- **Grammar:** Must be complete sentences starting with the name of the identifier.
- **Example:**
  ```go
  // Client manages communication with the API.
  type Client struct {}

  // Close terminates the underlying connection.
  func (c *Client) Close() error { return nil }