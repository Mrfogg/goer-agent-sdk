module github.com/excelmatic/goer-agent-sdk

go 1.23

require github.com/sashabaranov/go-openai v1.42.0

// The SDK needs ChatCompletionRequest.ExtraBody and the OpenAI-style `reasoning`
// field, which only exist in this fork. The fork declares the upstream module
// path (github.com/sashabaranov/go-openai), so it can only be selected through
// the replace below. Replace directives are NOT inherited by consumers: every
// module that imports this SDK must repeat this replace line in its own
// go.mod, or the build will fail on the missing fields.
replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
