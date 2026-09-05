# DeepSeek non-stream compatibility design and implementation plan

Goal: preserve non-stream Chat Completions clients while using working streaming upstream requests for the affected model.

Approved design: user approved buffering upstream streaming responses into complete JSON. Alternatives (client configuration changes or blind retries) do not transparently repair downstream clients. The feature is opt-in per account and exact upstream model via `extra.openai_chat_nonstream_via_stream_models`; initially enable only account 1 / deepseek-v4-flash-0731. Do not invent a context threshold, truncate prompts, change models, or bypass upstream errors.

Implementation:
1. Add regression tests in `backend/internal/service/openai_gateway_chat_completions_buffered_test.go`; demonstrate current SSE passthrough breaks the non-stream response contract.
2. Modify raw CC forwarding to independently select upstream streaming and downstream buffering. Add bounded SSE aggregation in `openai_gateway_chat_completions_buffered.go`, then reuse existing JSON response and billing extraction. Preserve content, reasoning, tool fragments, finish reasons and usage. Reject malformed, incomplete or missing-usage streams before writing a success response.
3. Test opt-in isolation, multi-choice/tool aggregation, context errors, interrupted streams, size limits and billing usage. Run existing raw CC tests with `go test -tags unit ./internal/service -run 'Test(ForwardAsRawChatCompletions|RawChatBuffered|BufferRawChatCompletions)'`.
4. Build an image preserving the current frontend and runtime, start a candidate on an unused loopback port with identical service configuration, and verify a real long non-stream request returns JSON with usage.
5. Back up runtime and account setting; activate only the affected model/account, switch Caddy after candidate checks, verify live request and usage records. Preserve rollback to old image and saved setting. Keep pre-existing source changes intact.

Isolation: `/home/ubuntu/worldcodes-builds/deepseek-nonstream-20260905`, branch `codex/fix-deepseek-nonstream` from production d78eed6ea. Existing frontend translation edit in the production checkout is preserved.
