// SPDX-License-Identifier: CC0-1.0
package b

import "example.com/cleanc-fixture-go/pkg/a"

type BBase struct{}
type BMid struct{ BBase }
type BLeaf struct{ BMid }

func ValueB() int { return duplicateBOne(4) }
func UseA() int { return a.UseB() }

func duplicateBOne(seed int) int {
total := seed
for i := 0; i < 8; i++ {
total += i * 2
total -= i / 2
total += 3
}
if total%2 == 0 {
total += 11
} else {
total += 17
}
return total
}