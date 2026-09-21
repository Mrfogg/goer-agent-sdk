module github.com/Mrfogg/goer-agent-sdk

go 1.23

require (
	github.com/glebarez/sqlite v1.11.0
	github.com/sashabaranov/go-openai v1.42.0
	gorm.io/gorm v1.31.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-isatty v0.0.17 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.7.0 // indirect
	golang.org/x/text v0.20.0 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)

// The SDK needs ChatCompletionRequest.ExtraBody and the OpenAI-style `reasoning`
// field, which only exist in this fork. The fork declares the upstream module
// path (github.com/sashabaranov/go-openai), so it can only be selected through
// the replace below. Replace directives are NOT inherited by consumers: every
// module that imports this SDK must repeat this replace line in its own
// go.mod, or the build will fail on the missing fields.
replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
