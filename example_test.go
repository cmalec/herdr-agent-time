package main

import "fmt"

// Example_formatDuration shows the sidebar format before and after one hour.
func Example_formatDuration() {
	fmt.Println(formatDuration(65_000))
	fmt.Println(formatDuration(3_661_000))
	// Output:
	// 1:05
	// 1:01:01
}
