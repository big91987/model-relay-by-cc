// 受控假上游：用于回归测试和账务边界验证，不能代替真实上游验收。
package main

import (
	"log"
	"net/http"
	"os"

	"modelrelay/internal/mockupstream"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":3901"
	}
	log.Printf("假上游已启动：http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mockupstream.New()))
}
