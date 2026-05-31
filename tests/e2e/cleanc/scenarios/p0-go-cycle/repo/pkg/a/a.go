// SPDX-License-Identifier: CC0-1.0
package a

import "example.com/cleanc-fixture-go/pkg/b"

type WideInterface interface {
M01() int
M02() int
M03() int
M04() int
M05() int
M06() int
M07() int
M08() int
M09() int
M10() int
M11() int
M12() int
M13() int
M14() int
M15() int
M16() int
M17() int
M18() int
M19() int
M20() int
}

type Base struct{}
type Mid struct{ Base }
type Leaf struct{ Mid }

type WideClass struct{}

func (WideClass) M01() int { return duplicateOne(1) }
func (WideClass) M02() int { return duplicateOne(2) }
func (WideClass) M03() int { return duplicateOne(3) }
func (WideClass) M04() int { return duplicateOne(4) }
func (WideClass) M05() int { return duplicateOne(5) }
func (WideClass) M06() int { return duplicateOne(6) }
func (WideClass) M07() int { return duplicateOne(7) }
func (WideClass) M08() int { return duplicateOne(8) }
func (WideClass) M09() int { return duplicateOne(9) }
func (WideClass) M10() int { return duplicateOne(10) }
func (WideClass) M11() int { return duplicateOne(11) }
func (WideClass) M12() int { return duplicateOne(12) }
func (WideClass) M13() int { return duplicateOne(13) }
func (WideClass) M14() int { return duplicateOne(14) }
func (WideClass) M15() int { return duplicateOne(15) }
func (WideClass) M16() int { return duplicateOne(16) }

func UseB() int { return b.ValueB() }

func duplicateOne(seed int) int {
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

func duplicateTwo(seed int) int {
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

func duplicateThree(seed int) int {
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