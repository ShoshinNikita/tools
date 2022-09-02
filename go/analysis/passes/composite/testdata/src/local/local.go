package local

import "flag"

type X struct {
	A int
	B string
}

func _() {
	_ = X{1, "hello"} // want "unkeyed fields"
}

var badStructLiteral = flag.Flag{ // want "unkeyed fields"
	"Name",
	"Usage",
	nil, // Value
	"DefValue",
}
