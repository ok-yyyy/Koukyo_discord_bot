# バイナリ名
BINARY_NAME=bot.exe

# Go コマンド
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GORUN=$(GOCMD) run

# メインファイルのパス
MAIN_PATH=./cmd/bot

.PHONY: all build clean test run lint

all: test build

build:
	$(GOBUILD) -o $(BINARY_NAME) -v $(MAIN_PATH)

test:
	$(GOTEST) -v ./...

clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)

run:
	$(GORUN) $(MAIN_PATH)

# go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest が必要
lint:
	golangci-lint run
