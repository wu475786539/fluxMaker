package num

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Decimal is an exact rational decimal used for prices, quantities and money.
// Its zero value is valid. It deliberately avoids float64 in trading paths.
type Decimal struct {
	r *big.Rat
}

func Parse(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("empty decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	return Decimal{r: r}, nil
}

func Must(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

func FromRat(r *big.Rat) Decimal {
	if r == nil {
		return Decimal{}
	}
	return Decimal{r: new(big.Rat).Set(r)}
}

func FromInt64(v int64) Decimal {
	return Decimal{r: new(big.Rat).SetInt64(v)}
}

// Cached read-only constants for hot paths. Every Decimal operation returns a
// fresh value and no method mutates the receiver's big.Rat in place, so these
// shared instances are safe for concurrent reads and avoid a big.Rat
// allocation on each use (quote generation and basis-point math hit them
// thousands of times per cycle).
var (
	one         = Decimal{r: big.NewRat(1, 1)}
	tenThousand = Decimal{r: big.NewRat(10_000, 1)}
)

// One returns the decimal value 1 without allocating.
func One() Decimal { return one }

// TenThousand returns the decimal value 10000 without allocating; basis-point
// math divides or multiplies by it frequently.
func TenThousand() Decimal { return tenThousand }

func (d Decimal) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

func (d Decimal) Rat() *big.Rat     { return new(big.Rat).Set(d.rat()) }
func (d Decimal) IsZero() bool      { return d.rat().Sign() == 0 }
func (d Decimal) IsPositive() bool  { return d.rat().Sign() > 0 }
func (d Decimal) Sign() int         { return d.rat().Sign() }
func (d Decimal) Cmp(o Decimal) int { return d.rat().Cmp(o.rat()) }

func (d Decimal) Add(o Decimal) Decimal { return FromRat(new(big.Rat).Add(d.rat(), o.rat())) }
func (d Decimal) Sub(o Decimal) Decimal { return FromRat(new(big.Rat).Sub(d.rat(), o.rat())) }
func (d Decimal) Mul(o Decimal) Decimal { return FromRat(new(big.Rat).Mul(d.rat(), o.rat())) }

func (d Decimal) Div(o Decimal) Decimal {
	if o.IsZero() {
		panic("decimal division by zero")
	}
	return FromRat(new(big.Rat).Quo(d.rat(), o.rat()))
}

func (d Decimal) Abs() Decimal {
	return FromRat(new(big.Rat).Abs(d.rat()))
}

func (d Decimal) Neg() Decimal {
	return FromRat(new(big.Rat).Neg(d.rat()))
}

func (d Decimal) Min(o Decimal) Decimal {
	if d.Cmp(o) <= 0 {
		return d
	}
	return o
}

func (d Decimal) Max(o Decimal) Decimal {
	if d.Cmp(o) >= 0 {
		return d
	}
	return o
}

// QuantizeDown rounds a non-negative value down to an exact step.
func (d Decimal) QuantizeDown(step Decimal) Decimal {
	if step.Sign() <= 0 || d.Sign() < 0 {
		panic("QuantizeDown requires non-negative value and positive step")
	}
	q := new(big.Rat).Quo(d.rat(), step.rat())
	n := new(big.Int).Quo(q.Num(), q.Denom())
	return FromRat(new(big.Rat).Mul(new(big.Rat).SetInt(n), step.rat()))
}

// QuantizeUp rounds a non-negative value up to an exact step.
func (d Decimal) QuantizeUp(step Decimal) Decimal {
	if step.Sign() <= 0 || d.Sign() < 0 {
		panic("QuantizeUp requires non-negative value and positive step")
	}
	q := new(big.Rat).Quo(d.rat(), step.rat())
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(q.Num(), q.Denom(), rem)
	if rem.Sign() != 0 {
		n.Add(n, big.NewInt(1))
	}
	return FromRat(new(big.Rat).Mul(new(big.Rat).SetInt(n), step.rat()))
}

func (d Decimal) Fixed(scale int) string { return d.rat().FloatString(scale) }

func (d Decimal) String() string {
	if d.r == nil || d.r.Sign() == 0 {
		return "0"
	}
	s := d.r.FloatString(18)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func (d Decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Decimal) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*d = Decimal{}
		return nil
	}
	var s string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
	} else {
		s = string(data)
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
