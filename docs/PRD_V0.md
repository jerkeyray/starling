# Starling — PRD v2
> event-sourced agent runtime for Go  
> status: pre-build / design phase  
> last updated: apr 2026

---

## 1. one-line definition

Starling is an event-sourced agent runtime for Go. Every agent run is recorded as an append-only log of events, making every execution deterministically replayable, cost-enforceable, and cryptographically auditable.

---

## 2. problem

Go backend teams have no production-grade option for running LLM agents inside their services. Current options fall into three buckets:

**Python frameworks** (LangChain, LangGraph) — force a second runtime, introduce operational complexity, break Go teams' observability and deployment conventions.

**Existing Go frameworks** (Eino, Google ADK, langchaingo, Genkit Go) — feature-complete but designed as state machines, not event logs. No deterministic replay, no streaming-level cost control, no provable audit trail. Several are tied to specific cloud ecosystems (Google, ByteDance).

**Rolling your own** — viable for a toy, infeasible for production.

The specific production pain points all of these leave unsolved:
- An agent loops or misbehaves in production. You have logs but can't reproduce the exact execution locally.
- An LLM call costs $30 before a runaway loop is detected. Your framework "monitors" cost but can't kill the call mid-stream.
- You need to prove to a compliance audit exactly what steps an agent took. You have partial logs, not evidence.
- A model provider deprecates a model. You can't verify your agent still works against historical behavior.

---

## 3. what Starling is

A Go library that runs LLM-driven agent loops with an event log as the core data structure. State is derived by replaying the log. Every side effect — LLM call, tool invocation, timing event — is an event.

Users import it as a Go package. They define tools, pick a provider, and start runs. Starling handles the loop, records everything, persists the log, and makes every run replayable.

---

## 4. what Starling is NOT

- An LLM application framework (not trying to be LangChain)
- A platform or service (library only, no central server)
- A multi-agent coordination framework (Eino / ADK own this)
- A RAG pipeline (users write a retrieval tool if needed)
- A prompt template system
- A visual / no-code builder

---

## 5. target user

A Go backend engineer who:
- Has an existing production Go service
- Needs to add agent capabilities inside that service
- Cares about debuggability, cost control, and auditability
- Does not want to maintain a Python sidecar
- Values simplicity over feature breadth

Concrete examples:
- Platform engineer at a fintech adding an agent that drafts customer communication — needs audit trail for compliance
- SRE adding an agent that triages alerts — needs replay for debugging when the agent misclassifies
- Backend engineer at a B2B SaaS adding an agent that processes support tickets — needs cost budgets per tenant

---

## 6. core design principles

1. **Event log is the architectural center.** Not a feature, not an optional persistence layer. Every run is a log. State is derived.

2. **Determinism is enforced.** Non-deterministic operations (time, random, external calls) go through the runtime, not directly in user code. Modeled after Temporal's constraint.

3. **Small API surface.** Fewer than ten exported types in the core package. Every addition requires justification.

4. **Go-idiomatic.** `context.Context` threaded everywhere. Explicit errors. Small interfaces. No magic.

5. **Provider and tool agnostic.** Interfaces, not integrations.

6. **Embeddable.** Runs inside an existing HTTP handler, gRPC service, or CLI tool. No side processes required.

---

## 7. core architecture

### 7.1 the event log

Every agent execution produces a sequence of events. Types include:

```
RunStarted
LLMCallScheduled
LLMCallCompleted     (includes full response, token count, cost)
ToolCallScheduled
ToolCallCompleted    (includes input, output, duration)
ToolCallFailed
ContextTruncated
BudgetExceeded
RunCompleted
RunFailed
RunCancelled
```

Events are immutable, timestamped, content-addressed (hash-chained), and written to the log via walrus (the WAL implementation).

### 7.2 determinism boundary

Following Temporal's model: the **agent loop itself must be deterministic**. All non-determinism flows through the runtime:

- Time: `runtime.Now()` records the time as an event, replay returns recorded value
- Random: `runtime.Random()` same pattern
- LLM calls: always through `runtime.LLMCall()` — records result, replay returns recorded
- Tool calls: always through `runtime.CallTool()` — same

User code that writes tools and configures agents is non-deterministic and runs as "side effects" (Temporal calls these Activities). The agent's decision loop is deterministic and replayable.

### 7.3 replay

Given a run ID:
1. Load the full event log for that run
2. Execute the agent loop
3. Instead of making real LLM calls or tool calls, return the recorded results from the log
4. Verify the sequence of commands generated matches the log — if not, flag as non-determinism error
5. Resume from the point of divergence (or, for debugging, step through interactively)

### 7.4 streaming cost enforcement

Every LLM call is wrapped by a cost tracker that:
- Counts tokens as they arrive in the stream
- Checks against the run's budget after every chunk
- If budget is exceeded mid-stream, cancels the HTTP context immediately
- Writes a `BudgetExceeded` event to the log
- Returns a typed error with partial results

### 7.5 core interfaces (draft)

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

type Provider interface {
    Complete(ctx context.Context, req *Request) (*Response, error)
    Stream(ctx context.Context, req *Request) (EventStream, error)
}

type EventLog interface {
    Append(ctx context.Context, runID string, event Event) error
    Read(ctx context.Context, runID string) ([]Event, error)
    Stream(ctx context.Context, runID string) (<-chan Event, error)
}

type Agent struct {
    Provider Provider
    Tools    []Tool
    Log      EventLog
    Budget   *Budget     // optional
    Config   Config
}

func (a *Agent) Run(ctx context.Context, goal string) (*RunResult, error)
func (a *Agent) Resume(ctx context.Context, runID string) (*RunResult, error)
func (a *Agent) Replay(ctx context.Context, runID string) (*RunResult, error)
func (a *Agent) Stream(ctx context.Context, goal string) (<-chan StepEvent, error)
```

---

## 8. features by milestone

### M1 — proof of concept (weeks 5-7)

Build the bare minimum that demonstrates the architecture works:

- Event types defined
- In-memory event log (no walrus yet)
- Anthropic provider only
- Sequential tool execution
- ReAct loop that writes every decision to the log
- Simple file-reading and HTTP tools for demo
- `Run()` works end-to-end

**Exit criteria:** An agent runs, finishes, and you can inspect the complete event log afterwards.

### M2 — replay works (weeks 8-10)

- walrus integration — log writes to disk
- `Replay(runID)` implemented
- Deterministic agent loop enforced
- `runtime.Now()`, `runtime.Random()` helpers
- Non-determinism detection during replay

**Exit criteria:** Run an agent. Kill the process. Call `Replay(runID)`. Agent replays with identical output.

### M3 — production features (weeks 11-14)

- OpenAI and Gemini providers
- Streaming with tool call support
- Parallel tool execution via errgroup
- Streaming-level cost budget enforcement
- Context cancellation propagation
- Context window management (token counting, configurable truncation)
- OpenTelemetry spans for every event

**Exit criteria:** Feature parity with Eino's core agent runtime, plus cost budgets no one else has.

### M4 — polish and launch (weeks 15-18)

- MCP tool interface (native)
- Documentation site
- Demo agent (code analysis agent)
- Benchmarks vs langchaingo and Eino (focus on reliability metrics, not speed)
- README with concrete examples
- Public launch (HN, Reddit r/golang, Twitter)

**Exit criteria:** Repo is public, docs work, an external developer can go from zero to running agent in 10 minutes.

### M5 and beyond (post-launch, don't plan yet)

- Deterministic replay UI (local web server that reads the log)
- Branching from historical events (what-if analysis)
- Versioned workflows (handling code changes across replays)
- A/B testing agents against historical logs
- Cryptographic audit trails (merkle tree over event log)

---

## 9. explicit non-goals

Things Starling will never do:

- Prompt template management
- Vector store integrations
- RAG pipelines
- Document loaders
- Visual dashboard (core library only)
- Hosted service (v0 is OSS library)
- Multi-agent graph composition
- Replace Temporal for general-purpose workflow orchestration

Things to resist adding in response to community requests:

- "Can we support X database?" → write a Tool
- "Can we add LangChain compatibility?" → no
- "Can we have a visual builder?" → no
- "Can we support voice?" → write a Tool

The power of a small framework comes from ruthless scope discipline.

---

## 10. demo agent

### the code analysis agent

Ships with the framework as `examples/code-review`.

**What it does:**
1. Accepts a GitHub repo URL
2. Lists the file tree
3. Reads key files in parallel (multiple tool calls)
4. Runs static analysis on each
5. Writes a structured report to `report.md`

**Why this demo:**
- Fully automated end-to-end (no human input mid-run)
- Uses parallel tool calls naturally
- Demonstrates resumability (kill it mid-analysis, resume)
- Audience for the framework (Go engineers) instantly understands the output
- Shows the moat features — kill it, replay the event log, get identical output

**Tools needed:** `fetch_file`, `list_directory`, `run_linter`, `write_file`

---

## 11. success criteria

### technical
- [ ] Event log is the core data structure, not a bolt-on
- [ ] Replay works end-to-end with identical output
- [ ] Cost budget enforcement proven via benchmark (agent killed at exactly $0.50)
- [ ] Context window management guarantees no silent truncation
- [ ] API under 10 exported types in core package
- [ ] README gets someone to working agent in under 10 minutes

### community
- [ ] External contributor within 3 months of launch
- [ ] One blog post picked up on HN front page
- [ ] At least one team using it in production within 6 months
- [ ] Response time on issues under 48 hours for first 6 months

### career
- [ ] Used as primary portfolio anchor on resume
- [ ] Referenced in at least one job application conversation
- [ ] Either lands a role at a target company or becomes a credibility anchor that does

---

## 12. risks and mitigations

**Risk:** Eino or Google ADK ships deterministic replay as a strategic feature.  
**Mitigation:** First-mover advantage on the narrative. Ship the blog posts and demos that define "event-sourced agent runtime" as a concept. Be the reference implementation.

**Risk:** Scope creep — community asks for features that don't fit the narrow positioning.  
**Mitigation:** Publish the explicit non-goals (section 9). Point to them when declining. Write a "why Starling doesn't do X" post if needed.

**Risk:** Replay is hard and has subtle bugs.  
**Mitigation:** Invest heavily in property-based testing. Temporal has 6 years of experience with non-determinism bugs — read their issue tracker.

**Risk:** The event log becomes a performance bottleneck.  
**Mitigation:** walrus was designed for high-throughput append. Benchmark early. If needed, support pluggable backends (SQLite, Postgres) as the log store.

**Risk:** Spending too long on the framework and not enough on getting interviews.  
**Mitigation:** M1 must ship in 3 weeks. Launch publicly by M4. Apply to roles starting at M4, not waiting for M5.

---

## 13. naming decisions

- **Project name:** Starling
- **Tagline:** event-sourced agent runtime for Go
- **Go module:** `github.com/jerkeyray/starling` (or equivalent)
- **GitHub org:** personal account initially, org later if it grows

---

## 14. references and influences

These are the projects to study deeply before building. Each has influenced a specific design choice.

### primary references (read deeply)

**Temporal** — the event-sourced execution model, determinism constraint, replay semantics. Every architectural decision in Starling's runtime layer should be informed by Temporal's work. Read:
- Workflow Definition docs
- Event History walkthroughs (Go SDK version)
- Non-determinism and versioning docs

**Inngest** — durable execution applied to modern async workloads. Their engineering blog is the best public writing on this topic.

**walrus (your own WAL)** — the persistence layer. Re-read with agent events in mind.

### secondary references (study the API design)

**Eino (ByteDance)** — the most mature Go LLM framework. Study the `Agent`, `Tool`, `Model` interfaces. Note what feels right and what feels over-abstracted.

**Google ADK Go** — Google's take on a Go agent framework. Note the multi-agent patterns, skip the A2A protocol stuff.

**langchaingo** — study for what *not* to do. It's a port of LangChain's Python abstractions into Go and shows what that looks like.

**LangGraph (Python)** — has checkpointing and persistence. Worth understanding how they model checkpoints vs. how Starling will model the full event log.

### conceptual / academic references

**ReAct paper** (Yao et al., 2022, arxiv 2210.03629) — the reasoning-action loop every agent framework implements. Read once, understand the loop from first principles.

**Event Sourcing (Martin Fowler)** — the architectural pattern Starling applies. Fowler's original essays on event sourcing, CQRS, and audit logs are the reference.

**Temporal blog — "Designing a Workflow Engine from First Principles"** — explains why event sourcing is the right model for durable execution.

**MCP spec** (modelcontextprotocol.io) — the tool interface standard.

### go-specific references

**Concurrency in Go** (Katherine Cox-Buday) — chapters 4-5 on errgroup, context, structured concurrency. Directly applies to parallel tool execution.

**100 Go Mistakes** (Teiva Harsanyi) — focus on mistakes around interface design and concurrency. Avoid these in Starling's public API.

**Bifrost source code** (you worked on it) — re-read with agent execution in mind. The provider abstraction is directly relevant.

### observability references

**OpenTelemetry Go SDK** — how to instrument a library (not an app) with OTel.

**Langfuse architecture writeup** — open-source LLM observability. Useful for thinking about what events to record and how to structure them.

---

## 15. open questions (decide before building)

These require decisions you can't outsource:

- **Is the event log a library detail or a user-facing concept?** (recommend: user-facing, it's the value prop)
- **Should agents be stateless or stateful structs?** (recommend: stateless execution with externalized state in the log)
- **How do you handle provider API version changes?** (defer to M5 — log records the API version used)
- **MCP by default or optional?** (recommend: optional in M1, native in M4)
- **What's the default event log persistence?** (in-memory for M1, walrus for M2+, pluggable by M3)
- **How much of the Temporal determinism model to adopt?** (adopt fully for the agent loop; keep Tools non-deterministic like Temporal Activities)