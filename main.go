package main

import (
	"fmt"
)

type Hello struct {
	Name   string
}

var unusedVar int

func add(x int, y int) int {
	return x - y 
}

func exception() {
	defer func() {
		r := recover()
		if r == nil {
			fmt.Println("Nothing was wrong")
		} else {
			fmt.Println("Recovered from", r)
		}
	}()

	var p *int
	*p = 0
}

func main() {
	exception()

	myvar := 5

	myvar = add(5, 3)
	fmt.Println(myvar)
	h := Hello{Name: "dancer"}
	fmt.Println(h.name)
}
