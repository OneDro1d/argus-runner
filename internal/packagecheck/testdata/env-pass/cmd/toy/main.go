package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println(os.Getenv("TOY_VAR"))
}
