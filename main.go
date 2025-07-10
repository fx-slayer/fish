package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) == 1 {
		exit("no file provided.")
	}
	fn := os.Args[1]
	if !filepath.IsAbs(fn) {
		wd, e := os.Getwd()
		if e != nil {
			exit(e)
		}
		fn = filepath.Join(wd, fn)
	}

	r := NewReader(fn)
	if e := r.Run(); e != nil {
		exit(e)
	}
}

// exit gentle quit with any message.
func exit(i ...any) {
	if len(i) > 0 {
		switch i[0].(type) {
		case string:
			si := i[0].(string)
			fmt.Println(si)
		case error:
			ei := i[0].(error)
			fmt.Println(ei.Error())
		default:
			fmt.Println(i)
		}
	}
	os.Exit(0)
}
