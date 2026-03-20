.PHONY: build build-arm64 docker run test clean

build:
	go build -o offtube .

build-arm64:
	GOOS=linux GOARCH=arm64 go build -o offtube-arm64 .

docker:
	docker build -t offtube .

run:
	go run .

test:
	go test ./...

clean:
	rm -f offtube offtube-arm64
