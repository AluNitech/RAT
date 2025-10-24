package main

import "crypto/subtle"

// secureCompare は一定時間で比較を行いタイミング攻撃を防ぐ
func secureCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
