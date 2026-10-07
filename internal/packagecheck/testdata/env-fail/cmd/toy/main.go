package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println(os.Getenv("UNDECLARED_VAR"))
}
