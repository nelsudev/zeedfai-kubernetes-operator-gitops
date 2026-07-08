package main

import (
	"net/url"
	"testing"
)

func TestBoundedIntParam(t *testing.T) {
	tests := []struct {
		name    string
		values  url.Values
		want    int
		wantErr bool
	}{
		{name: "default", values: url.Values{}, want: 120},
		{name: "valid", values: url.Values{"seconds": {"30"}}, want: 30},
		{name: "not integer", values: url.Values{"seconds": {"abc"}}, wantErr: true},
		{name: "too low", values: url.Values{"seconds": {"0"}}, wantErr: true},
		{name: "too high", values: url.Values{"seconds": {"301"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := boundedIntParam(tt.values, "seconds", 120, 1, 300)
			if (err != nil) != tt.wantErr {
				t.Fatalf("boundedIntParam() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("boundedIntParam() = %d, want %d", got, tt.want)
			}
		})
	}
}
