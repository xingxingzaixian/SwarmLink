package app

import "encoding/base64"

// encodeBase64 / decodeBase64 是包内小工具，避免每个转换函数都重复 import。
func encodeBase64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decodeBase64(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
