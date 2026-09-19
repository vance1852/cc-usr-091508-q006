package api

import (
	"crypto/rand"
	"encoding/hex"
)

// randToken 生成服务端在线请求使用的随机幂等键。
func randToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
