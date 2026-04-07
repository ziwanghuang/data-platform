.PHONY: build server agent clean sql test

# 默认构建
build: server agent

server:
	go build -o bin/server ./cmd/server/

agent:
	go build -o bin/agent ./cmd/agent/

clean:
	rm -rf bin/

# 执行建表 SQL
sql:
	mysql -h 127.0.0.1 -u root -proot woodpecker < sql/schema.sql

# Step 5 用
proto:
	protoc --go_out=. --go-grpc_out=. proto/*.proto

# 运行测试
test:
	go test ./... -v -count=1
