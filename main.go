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
	wd, e := os.Getwd()
	if e != nil {
		exit(e)
	}
	r := Reader{
		f:        filepath.Join(wd, os.Args[1]),
		index:    []string{},
		progress: make(map[string]int),
	}
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
