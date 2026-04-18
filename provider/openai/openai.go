// Package openai adapts the OpenAI Chat Completions API (and
// OpenAI-compatible APIs — Azure, Groq, Together, OpenRouter, Ollama, vLLM,
// LM Studio, llama.cpp server, etc.) to Starling's Provider interface.
//
// OpenAI compatibility is unlocked by WithBaseURL; no separate adapters are
// needed for providers that mirror the Chat Completions API.
package openai
