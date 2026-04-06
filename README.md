# runtime

[![Go Reference](https://pkg.go.dev/badge/github.com/openotters/runtime.svg)](https://pkg.go.dev/github.com/openotters/runtime)
[![Go Report Card](https://goreportcard.com/badge/github.com/openotters/runtime)](https://goreportcard.com/report/github.com/openotters/runtime)
[![golangci-lint](https://github.com/openotters/runtime/actions/workflows/golangci.yml/badge.svg)](https://github.com/openotters/runtime/actions/workflows/golangci.yml)
[![License](https://img.shields.io/github/license/openotters/runtime)](LICENSE)

Single-agent gRPC runtime for OpenOtters. Runs one agent with tools, memory, and multi-session support.

## Overview

The runtime is a standalone server that:

1. Reads `etc/agent.yaml` for agent configuration (name, model, tools)
2. Loads context files from `etc/context/` to build the system prompt
3. Discovers and loads tool binaries from `usr/bin/`
4. Serves a gRPC API for chat, streaming, session management, and health checks
5. Persists conversation history to SQLite with configurable compaction

In production, each Agent CR creates its own runtime Pod (via the operator). For development, it runs standalone against a materialized agent directory (produced by the [agentfile](https://github.com/openotters/agentfile) executor).

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

The runtime reads `etc/agent.yaml` from `--root` automatically. No need to pass `--name`, `--model`, or `--tools` explicitly.

## Agent Root Layout

The runtime expects an FHS-inspired directory produced by the agentfile executor:

```
{root}/
├── etc/
│   ├── agent.yaml       # name, model, tool definitions
│   ├── context/         # .md files → system prompt
│   └── data/            # data files (tool working directory)
├── usr/bin/             # tool binaries
├── workspace/           # agent working directory
├── tmp/                 # scratch space
└── var/lib/memory.db    # SQLite conversation store
```

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
