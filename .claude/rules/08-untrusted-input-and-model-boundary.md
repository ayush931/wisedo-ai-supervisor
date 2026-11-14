# Rule 08 — Untrusted input and the model boundary

Source: WIS-275, WIS-276, WIS-280, WIS-271 "Parser and upload defaults", `SECURITY.md`.

Source content is data. Model output stays hostile even when it is valid JSON — schema compliance
is not prompt-injection proof. The boundary is deterministic allowlists, scoped reads and effect checks.

## Required

- Models may request only `context.get`, `evidence.get`, `claims.search` and `measurements.get`,
  within scopes captured server-side. Retrieval IDs are allowlisted against tenant/site/version
  at execution time.
- Each tool receives its server context separately; requested tenant or actor fields are rejected.
  Child authority is the intersection of parent, role and current policy.
- A model proposal is returned as data; deterministic application code validates it and only then
  submits it for approval. Invented or cross-tenant citation IDs reject the whole output.
- Budgets are reserved atomically before parallel calls and charged afterwards. Unknown model
  pricing disables a billable call until versioned prices are configured.
- Contradictory specialist evidence becomes `needs_input`, never a majority vote.
- Document parsing runs without network access, without tools, under bounded CPU/memory/time;
  failed parses quarantine with an explicit reason. Reject encrypted PDFs, archives, executables,
  unsupported MIME, external relationships and decompression bombs. File names are display-only;
  object keys are server-generated.
- Cancellation generation is checked before and after every tool call and before artifact commit.

## Model endpoints (WIS-622, decided 2026-11-05)

Two endpoint classes exist. The default is the first; the second is opt-in per tenant.

1. **Local or private, address-pinned** (vLLM, llama.cpp, Ollama on the customer's or our host).
   Unchanged: the allowlist and pinned addresses in `internal/agent/localmodel.go`.
2. **Hosted, per site, off by default.** Qwen (Plus and Flash tiers) on Alibaba Cloud Model
   Studio, International (Singapore) region only. The switch is per site, not per tenant: owner
   authority is per site, so one site's owner can never decide for another site of the same
   tenant (stricter than the per-tenant wording of the WIS-622 decision). A site may use it only
   when all hold:
   - an owner of that site turned it on with a recorded reason and a recorded data processing
     agreement reference; turning it off takes effect on the next call, including queued work,
     and a run whose site was switched off stops with a stable reason (it never continues on
     another provider);
   - the host is on the allowlist compiled into the binary (not configuration), reached over
     HTTPS (TLS 1.2 or later, system trust store), with no proxy and no redirects, and a dial to
     a private or loopback address is refused; the API key is read from a file and never logged
     or returned;
   - people's names, customer names, site and tenant names, e-mail addresses and phone numbers
     known to the system are replaced by request-local tokens before the call. The response is
     restored at the adapter boundary from that request's own token map only; any token-shaped
     text that is not exactly an entry of the map refuses the whole output. The restored output
     is then validated server-side like any model output. This is dictionary pseudonymisation:
     a name the system does not know (for example in free SOP text) is not caught, and the DPA
     must reflect that;
   - the model and its revision have a price in the committed, versioned price table, and the
     call fits the run's reserved budget; an unknown price refuses the call before it is made;
   - a run is pinned to one provider, region, model, revision and price version when it is
     created and records them; one run never mixes providers or models.

Self-hosted embeddings and rerank (WIS-627) need no switch; hosted embeddings need the same
site switch as hosted chat. Model output from any endpoint is validated exactly as above.

## Forbidden

- Shell, SQL, browser, arbitrary HTTP or dynamic MCP discovery as agent tools.
- Scope tokens, credentials or system prompts travelling between agents or inside model-visible content.
- One model call per raw machine event — aggregate and deduplicate before investigation.
- Logging secrets or unredacted model prompts.
- Sending customer data to `api.deepseek.com`, to any host not on the compiled-in allowlist, or
  to a hosted endpoint for a site whose switch is off. A hosted call never falls back to another
  hosted provider; it fails visibly.
