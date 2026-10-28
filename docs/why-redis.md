# Why Redis

A language model charges you, and makes you wait, every time you send it a prompt. A lot of those prompts are repeats: the same support question, the same system prompt plus the same user text, a retry after a timeout, two users asking the same thing.

RediGate remembers the answer. Redis is the memory.

The first time a cacheable question arrives, RediGate calls the model and saves the reply. The next time that same question arrives, RediGate reads Redis and returns the saved reply. The model is not called, so you do not pay for it again, and the caller does not wait on it again.

This is an exact match, not a "close enough" match. "How do I reset my password?" and "How do I reset my password, please?" are different questions. Semantic search, embeddings, and vector databases are not part of this project.

## What is stored

Redis holds the model's raw response body, usually a JSON chat completion. The prompt itself is not the value. It is folded into a hash and used only as the lookup key.

Keys look like this:

```text
redigate:chat:v1:<sha256>
```

The hash is of a canonical document:

- `model`, with surrounding whitespace removed. Letter case stays, so `gpt-4o-mini` and `GPT-4o-mini` are different.
- `messages`, in order. `role` is trimmed and lowercased. String `content`, and the `text` of content parts, is trimmed. A blank `name` is dropped. Other message fields are kept.
- Every other top-level field except `temperature` and `stream`. `max_tokens`, `tools`, `n`, and similar fields each get their own cache entry, because they change the answer.

`temperature` and `stream` are not part of the hash. They decide whether RediGate is allowed to read or write the cache. A creative request cannot overwrite the stored answer for the temperature-zero request.

The `v1` in the key is the generation. If normalization ever changes, bumping that segment ignores old entries.

The original JSON body is what the model sees. Normalization is only for the key. Two requests that normalize to the same key share one saved answer: the first one to miss wins until the TTL expires. `{"role":"User","content":"  hi  "}` and `{"role":"user","content":"hi"}` are the same key.

## How long it stays

`CACHE_TTL_SECONDS` is the lifetime of each saved answer. The default is 3600 seconds (one hour). It has to be a positive integer. When the TTL passes, the next request is a miss and the model is called again.

The Compose file runs Redis with snapshots and append-only logs turned off. A restart of that Redis drops the cache. That is what you want for a cache: a cold start just means the next questions are misses.

## What is not cached

| Situation | `X-RediGate-Cache` | Saved? | Model called? |
| --- | --- | --- | --- |
| First cacheable request | `MISS` | Yes, if the reply qualifies | Yes |
| Same cacheable request again, before TTL | `HIT` | Already saved | No |
| `stream` true | `BYPASS` | No | Yes |
| `temperature` greater than 0 | `BYPASS` | No | Yes |
| Redis read fails | `MISS` | It still tries to save | Yes |
| Redis write fails | `MISS` | No | Yes |
| Model returns a non-200 (errors, rate limits) | `MISS` | No | Yes |
| Model returns 200 that is not JSON | `MISS` | No | Yes |
| Model omits Content-Type on a 200 | `MISS` | Yes, if the body is non-empty | Yes |
| Model returns an empty 200 body | `MISS` | No | Yes |
| Successful body larger than 8 MiB | `MISS` | No | Yes |
| Model cannot be reached | header omitted, HTTP 502 | No | Attempted |

Cacheable means all of the following:

- `stream` is absent, null, or false
- `temperature` is absent, null, or `0`

Omitted temperature is treated as zero for this decision only. RediGate does not add `"temperature": 0` to the body. If your provider's default temperature is not zero, that first call may be non-deterministic, and the saved reply will be whatever came back. Send `"temperature": 0` when the model call itself must be deterministic.

A negative temperature, a non-numeric temperature, or a non-boolean `stream` is rejected with HTTP 400. Those are not bypasses.

Streaming responses are copied straight through and flushed as they arrive. They are never stored. Two identical in-flight misses are also not joined into one model call yet. Each miss calls the model.

## When Redis is down

Two different behaviors:

- **At startup**, RediGate pings Redis and exits if the ping fails. Compose waits until Redis is healthy before starting the app, then restarts the app if that first ping still fails.
- **After startup**, a failed read or write does not fail the user's request. RediGate calls the model and returns that reply with `MISS`. The error is logged, not sent to the caller.

`GET /healthz` only means the process is up. It does not touch Redis. `GET /readyz` pings Redis and returns 503 when the ping fails.

Reads and writes give Redis about two seconds, then give up and follow the fail-open path.

## What you should not assume

Redis in the sample Compose file has no password, and the port is published on `127.0.0.1`. Saved answers are full model replies. Anyone who can open that Redis can read them. Do not publish port 6379 on a public network, and do not point RediGate at a shared Redis without access control.

There is no caller authentication on RediGate itself. Anyone who can reach `:8080` can spend your provider key. Put it behind a network you trust.

`.env` holds `UPSTREAM_API_KEY`. It is gitignored. `.env.example` does not contain a real key. Do not commit a filled-in `.env`.

RediGate does not log the provider key or the prompt. It logs the model name, the cache result, the status, and how long the call took.
