# runtime

[![Go Reference](https://pkg.go.dev/badge/github.com/openotters/runtime.svg)](https://pkg.go.dev/github.com/openotters/runtime)
[![Go Report Card](https://goreportcard.com/badge/github.com/openotters/runtime)](https://goreportcard.com/report/github.com/openotters/runtime)
[![golangci-lint](https://github.com/openotters/runtime/actions/workflows/golangci.yml/badge.svg)](https://github.com/openotters/runtime/actions/workflows/golangci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)

Single-agent gRPC runtime for OpenOtters. Runs one agent with tools, memory, and multi-session support.

<!-- TOC -->
* [runtime](#runtime)
  * [Overview](#overview)
  * [gRPC API](#grpc-api)
  * [Usage](#usage)
  * [Packages](#packages)
  * [Memory Compaction](#memory-compaction)
  * [License](#license)
<!-- TOC -->

## Overview

The runtime is a standalone gRPC server that implements the
[Agentfile specification](https://github.com/openotters/agentfile). It consumes agent directories materialized by
the [agentfile executor](https://github.com/openotters/agentfile) and runs them as long-lived services.

Given an agent root directory following the
[Agentfile filesystem layout](https://github.com/openotters/agentfile/blob/main/specs/AGENTFILE-v0.0.1.md#agent-filesystem-layout),
the runtime:

1. Reads `etc/agent.yaml` for agent configuration (name, model, tools)
2. Loads context files from `etc/context/` to build the system prompt
3. Discovers and loads tool binaries from `usr/bin/`
4. Serves a gRPC API for chat, streaming, session management, and health checks
5. Persists conversation history to SQLite with configurable compaction

## gRPC API

| RPC             | Description                                                              |
|-----------------|--------------------------------------------------------------------------|
| `Chat`          | Send a prompt, get a response                                            |
| `ChatStream`    | Send a prompt, receive streaming events (steps, tool calls, text deltas) |
| `ListSessions`  | List active conversation sessions                                        |
| `DeleteSession` | Delete a session and its history                                         |
| `Health`        | Health check (agent name, model)                                         |
| `Ready`         | Readiness probe                                                          |

Proto definition: [`api/v1/runtime.proto`](api/v1/runtime.proto)

## Usage

```sh
# Start the gRPC server
runtime serve --root /path/to/agent --api-key $ANTHROPIC_API_KEY

# Send a one-shot prompt (debug)
runtime prompt --root /path/to/agent --api-key $ANTHROPIC_API_KEY "What is the weather?"
```

The runtime reads `etc/agent.yaml` from `--root` automatically. No need to pass `--name`, `--model`, or `--tools`
explicitly.

## Packages

| Package        | Description                                                     |
|----------------|-----------------------------------------------------------------|
| `pkg/agent`    | Agent creation (provider selection, LLM setup) and chat service |
| `pkg/memory`   | SQLite message store and history compaction (sliding/summarize) |
| `pkg/tool`     | Tool binary loading and JSON stdin/stdout execution             |
| `pkg/neighbor` | Inter-agent communication via HTTP                              |

## Memory Compaction

Configured via `etc/agent.yaml` configs:

| Key                   | Default     | Description              |
|-----------------------|-------------|--------------------------|
| `memory-strategy`     | `summarize` | `sliding` or `summarize` |
| `memory-max-messages` | `20`        | Trigger threshold        |

- **summarize**: sends oldest messages to the LLM for summarization, keeps recent half
- **sliding**: drops oldest messages, keeps the last N

## License

See [LICENSE](LICENSE.md).
