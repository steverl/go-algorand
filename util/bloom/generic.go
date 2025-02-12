// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package bloom

// GenericFilter is the interface for either bloom.Filter or bloom.XorFilter
type GenericFilter interface {
	// The input x is expected to be a slice with a length of 8 bytes or more.
	Set(x []byte)
	// The input x is expected to be a slice with a length of 8 bytes or more.
	Test(x []byte) bool
	MarshalBinary() ([]byte, error)
	UnmarshalBinary(data []byte) error
}
