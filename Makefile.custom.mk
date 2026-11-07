##@ Docs

.PHONY: tools-doc
tools-doc: ## Regenerate docs/tools.md from the registered tools (the test TestToolsDocIsCurrent keeps it current).
	TOOLS_DOC_UPDATE=1 go test -count=1 -run TestToolsDocIsCurrent ./cmd
