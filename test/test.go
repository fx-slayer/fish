package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	sb := strings.Builder{}
	for i := 0; i < 976; i++ {
		sb.WriteString(fmt.Sprintf("%d\n", i))
	}
	e := os.WriteFile("num.txt", []byte(sb.String()), 0644)
	if e != nil {
		panic(e)
	}
}
