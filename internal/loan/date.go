package loan

import (
	"fmt"
	"time"
)

const DateLayout = "2006-01-02"

type Date struct {
	year  int
	month time.Month
	day   int
}

func NewDate(value time.Time, location *time.Location) Date {
	if location == nil {
		location = time.UTC
	}
	value = value.In(location)
	return Date{year: value.Year(), month: value.Month(), day: value.Day()}
}

func ParseDate(raw string, location *time.Location) (Date, error) {
	if location == nil {
		return Date{}, fmt.Errorf("date location is required")
	}
	value, err := time.ParseInLocation(DateLayout, raw, location)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", raw, err)
	}
	return NewDate(value, location), nil
}

func (date Date) IsZero() bool { return date.year == 0 }

func (date Date) Time(location *time.Location) time.Time {
	if location == nil {
		location = time.UTC
	}
	return time.Date(date.year, date.month, date.day, 0, 0, 0, 0, location)
}

func (date Date) String() string {
	if date.IsZero() {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", date.year, date.month, date.day)
}

func (date Date) Before(other Date) bool { return date.key() < other.key() }
func (date Date) After(other Date) bool  { return date.key() > other.key() }
func (date Date) Equal(other Date) bool  { return date.key() == other.key() }

func (date Date) key() int { return date.year*10000 + int(date.month)*100 + date.day }

func (date Date) AddDays(days int, location *time.Location) Date {
	return NewDate(date.Time(location).AddDate(0, 0, days), location)
}
