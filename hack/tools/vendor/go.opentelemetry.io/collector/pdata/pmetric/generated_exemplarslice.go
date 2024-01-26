// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Code generated by "pdata/internal/cmd/pdatagen/main.go". DO NOT EDIT.
// To regenerate this file run "make genpdata".

package pmetric

import (
	"go.opentelemetry.io/collector/pdata/internal"
	otlpmetrics "go.opentelemetry.io/collector/pdata/internal/data/protogen/metrics/v1"
)

// ExemplarSlice logically represents a slice of Exemplar.
//
// This is a reference type. If passed by value and callee modifies it, the
// caller will see the modification.
//
// Must use NewExemplarSlice function to create new instances.
// Important: zero-initialized instance is not valid for use.
type ExemplarSlice struct {
	orig  *[]otlpmetrics.Exemplar
	state *internal.State
}

func newExemplarSlice(orig *[]otlpmetrics.Exemplar, state *internal.State) ExemplarSlice {
	return ExemplarSlice{orig: orig, state: state}
}

// NewExemplarSlice creates a ExemplarSlice with 0 elements.
// Can use "EnsureCapacity" to initialize with a given capacity.
func NewExemplarSlice() ExemplarSlice {
	orig := []otlpmetrics.Exemplar(nil)
	state := internal.StateMutable
	return newExemplarSlice(&orig, &state)
}

// Len returns the number of elements in the slice.
//
// Returns "0" for a newly instance created with "NewExemplarSlice()".
func (es ExemplarSlice) Len() int {
	return len(*es.orig)
}

// At returns the element at the given index.
//
// This function is used mostly for iterating over all the values in the slice:
//
//	for i := 0; i < es.Len(); i++ {
//	    e := es.At(i)
//	    ... // Do something with the element
//	}
func (es ExemplarSlice) At(i int) Exemplar {
	return newExemplar(&(*es.orig)[i], es.state)
}

// EnsureCapacity is an operation that ensures the slice has at least the specified capacity.
// 1. If the newCap <= cap then no change in capacity.
// 2. If the newCap > cap then the slice capacity will be expanded to equal newCap.
//
// Here is how a new ExemplarSlice can be initialized:
//
//	es := NewExemplarSlice()
//	es.EnsureCapacity(4)
//	for i := 0; i < 4; i++ {
//	    e := es.AppendEmpty()
//	    // Here should set all the values for e.
//	}
func (es ExemplarSlice) EnsureCapacity(newCap int) {
	es.state.AssertMutable()
	oldCap := cap(*es.orig)
	if newCap <= oldCap {
		return
	}

	newOrig := make([]otlpmetrics.Exemplar, len(*es.orig), newCap)
	copy(newOrig, *es.orig)
	*es.orig = newOrig
}

// AppendEmpty will append to the end of the slice an empty Exemplar.
// It returns the newly added Exemplar.
func (es ExemplarSlice) AppendEmpty() Exemplar {
	es.state.AssertMutable()
	*es.orig = append(*es.orig, otlpmetrics.Exemplar{})
	return es.At(es.Len() - 1)
}

// MoveAndAppendTo moves all elements from the current slice and appends them to the dest.
// The current slice will be cleared.
func (es ExemplarSlice) MoveAndAppendTo(dest ExemplarSlice) {
	es.state.AssertMutable()
	dest.state.AssertMutable()
	if *dest.orig == nil {
		// We can simply move the entire vector and avoid any allocations.
		*dest.orig = *es.orig
	} else {
		*dest.orig = append(*dest.orig, *es.orig...)
	}
	*es.orig = nil
}

// RemoveIf calls f sequentially for each element present in the slice.
// If f returns true, the element is removed from the slice.
func (es ExemplarSlice) RemoveIf(f func(Exemplar) bool) {
	es.state.AssertMutable()
	newLen := 0
	for i := 0; i < len(*es.orig); i++ {
		if f(es.At(i)) {
			continue
		}
		if newLen == i {
			// Nothing to move, element is at the right place.
			newLen++
			continue
		}
		(*es.orig)[newLen] = (*es.orig)[i]
		newLen++
	}
	// TODO: Prevent memory leak by erasing truncated values.
	*es.orig = (*es.orig)[:newLen]
}

// CopyTo copies all elements from the current slice overriding the destination.
func (es ExemplarSlice) CopyTo(dest ExemplarSlice) {
	dest.state.AssertMutable()
	srcLen := es.Len()
	destCap := cap(*dest.orig)
	if srcLen <= destCap {
		(*dest.orig) = (*dest.orig)[:srcLen:destCap]
	} else {
		(*dest.orig) = make([]otlpmetrics.Exemplar, srcLen)
	}
	for i := range *es.orig {
		newExemplar(&(*es.orig)[i], es.state).CopyTo(newExemplar(&(*dest.orig)[i], dest.state))
	}
}