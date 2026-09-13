package loan

import (
	"database/sql/driver"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var decimalPattern = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)

// Money is an immutable exact decimal/rational value. Its zero value is zero.
type Money struct {
	value *big.Rat
}

func ParseMoney(raw string) (Money, error) {
	raw = strings.TrimSpace(raw)
	if !decimalPattern.MatchString(raw) {
		return Money{}, fmt.Errorf("invalid money %q", raw)
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return Money{}, fmt.Errorf("invalid money %q", raw)
	}
	return Money{value: value}, nil
}

func MustMoney(raw string) Money {
	value, err := ParseMoney(raw)
	if err != nil {
		panic(err)
	}
	return value
}

func MoneyFromInt(value int64) Money {
	return Money{value: new(big.Rat).SetInt64(value)}
}

func (money Money) rat() *big.Rat {
	if money.value == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Set(money.value)
}

func (money Money) Add(other Money) Money {
	return Money{value: new(big.Rat).Add(money.rat(), other.rat())}
}

func (money Money) Sub(other Money) Money {
	return Money{value: new(big.Rat).Sub(money.rat(), other.rat())}
}

func (money Money) Mul(other Money) Money {
	return Money{value: new(big.Rat).Mul(money.rat(), other.rat())}
}

func (money Money) DivInt(divisor int64) (Money, error) {
	if divisor == 0 {
		return Money{}, fmt.Errorf("divide money by zero")
	}
	return Money{value: new(big.Rat).Quo(money.rat(), new(big.Rat).SetInt64(divisor))}, nil
}

func (money Money) Neg() Money { return Money{value: new(big.Rat).Neg(money.rat())} }

func (money Money) Cmp(other Money) int { return money.rat().Cmp(other.rat()) }
func (money Money) IsZero() bool        { return money.Cmp(Money{}) == 0 }
func (money Money) IsNegative() bool    { return money.Cmp(Money{}) < 0 }
func (money Money) IsPositive() bool    { return money.Cmp(Money{}) > 0 }

func MinMoney(left, right Money) Money {
	if left.Cmp(right) <= 0 {
		return left
	}
	return right
}

func (money Money) Format(scale int) string {
	if scale < 0 {
		scale = 0
	}
	return money.rat().FloatString(scale)
}

func (money Money) String() string {
	value := money.Format(10)
	value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
	if value == "" || value == "-0" {
		return "0"
	}
	return value
}

func (money *Money) Scan(source any) error {
	var raw string
	switch value := source.(type) {
	case nil:
		return fmt.Errorf("scan money from NULL")
	case string:
		raw = value
	case []byte:
		raw = string(value)
	case int64:
		*money = MoneyFromInt(value)
		return nil
	default:
		return fmt.Errorf("scan money from %T", source)
	}
	parsed, err := ParseMoney(raw)
	if err != nil {
		return err
	}
	*money = parsed
	return nil
}

func (money Money) Value() (driver.Value, error) {
	return money.Format(10), nil
}
