// Copyright (c) 2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package standalone

import (
	"github.com/Decred-Next/dcrnd/chaincfg/chainhash/v8"
)

// log2FloorMasks defines the masks to use when quickly calculating
// floor(log2(x)) in a constant log2(32) = 5 steps, where x is a uint32, using
// shifts.  They are derived from (2^(2^x) - 1) * (2^(2^x)), for x in 4..0.
var log2FloorMasks = []uint32{0xffff0000, 0xff00, 0xf0, 0xc, 0x2}

// fastLog2Ceil calculates and returns ceil(log2(x)) in a constant 5 steps.
func fastLog2Ceil(n uint32) uint8 {
	// The final answer will be one more when the input is greater than one and
	// not a power of two.
	rv := uint8(0)
	if n&(n-1) != 0 {
		rv += 1
	}
	exponent := uint8(16)
	for i := 0; i < 5; i++ {
		if n&log2FloorMasks[i] != 0 {
			rv += exponent
			n >>= exponent
		}
		exponent >>= 1
	}
	return rv
}

// GenerateInclusionProof treats the provided slice of hashes as leaves of a
// merkle tree and generates and returns a merkle tree inclusion proof for the
// given leaf index.  The proof can be used to efficiently prove the leaf
// associated with given leaf index is a member of the tree.
//
// A merkle tree inclusion proof consists of the ceil(log2(x)) intermediate
// sibling hashes along the path from the target leaf to prove through the root
// node.  The sibling hashes, along with the original leaf hash (and its
// original leaf index), can be used to recalculate the merkle root which, in
// turn, can be verified against a known good merkle root in order to prove the
// leaf is actually a member of the tree at that position.
//
// For example, consider the following merkle tree:
//
//	         root = h1234 = h(h12 + h34)
//	        /                           \
//	  h12 = h(h1 + h2)            h34 = h(h3 + h4)
//	   /            \              /            \
//	  h1            h2            h3            h4
//
// Further, consider the goal is to prove inclusion of h3 at the 0-based leaf
// index of 2.  The proof will consist of the sibling hashes h4 and h12.  On the
// other hand, if the goal were to prove inclusion of h2 at the 0-based leaf
// index of 1, the proof would consist of the sibling hashes h1 and h34.
//
// Specifying a leaf index that is out of range will return nil.
func GenerateInclusionProof(leaves []chainhash.Hash, leafIndex uint32) []chainhash.Hash {
	if len(leaves) == 0 {
		return nil
	}

	if leafIndex >= uint32(len(leaves)) {
		return nil
	}

	// Copy the leaves so they can be safely mutated by the in-place merkle root
	// calculation.  Note that the backing array is provided with space for one
	// additional item when the number of leaves is odd as an optimization for
	// the in-place calculation to avoid the need grow the backing array.
	allocLen := len(leaves) + len(leaves)&1
	dupLeaves := make([]chainhash.Hash, len(leaves), allocLen)
	copy(dupLeaves, leaves)
	leaves = dupLeaves

	// Create a buffer to reuse for hashing the branches and some long lived
	// slices into it to avoid reslicing.
	var buf [2 * chainhash.HashSize]byte
	var left = buf[:chainhash.HashSize]
	var right = buf[chainhash.HashSize:]
	var both = buf[:]

	// The following algorithm works by replacing the leftmost entries in the
	// slice with the concatenations of each subsequent set of 2 hashes and
	// shrinking the slice by half to account for the fact that each level of
	// the tree is half the size of the previous one.  In the case a level is
	// unbalanced (there is no final right child), the final node is duplicated
	// so it ultimately is concatenated with itself.
	//
	// For example, the following illustrates calculating a tree with 5 leaves:
	//
	// [0 1 2 3 4]                              (5 entries)
	// 1st iteration: [h(0||1) h(2||3) h(4||4)] (3 entries)
	// 2nd iteration: [h(h01||h23) h(h44||h44)] (2 entries)
	// 3rd iteration: [h(h0123||h4444)]         (1 entry)
	//
	// Meanwhile, the intermediate sibling hashes for the branch opposite the
	// one that contains the leaf index at each level are stored to form the
	// proof.
	proofSize := fastLog2Ceil(uint32(len(leaves)))
	proof := make([]chainhash.Hash, 0, proofSize)
	for len(leaves) > 1 {
		// When there is no right child, the parent is generated by hashing the
		// concatenation of the left child with itself.
		if len(leaves)&1 != 0 {
			leaves = append(leaves, leaves[len(leaves)-1])
		}

		// Set the parent node to the hash of the concatenation of the left and
		// right children while storing the intermediate sibling hashes needed
		// to prove inclusion of the provided leaf index.
		halfLeafIndex := leafIndex >> 1
		for i := uint32(0); i < uint32(len(leaves)>>1); i++ {
			// The sibling hash needed to prove inclusion of the leaf at the
			// provided index is on the left when it is odd for this level.
			// Otherwise, it's on the right.  Thus, save the intermediate
			// sibling hashes that form the proof accordingly.
			leftLeaf := &leaves[i<<1]
			rightLeaf := &leaves[(i<<1)+1]
			if i == halfLeafIndex {
				if leafIndex&1 != 0 {
					proof = append(proof, *leftLeaf)
				} else {
					proof = append(proof, *rightLeaf)
				}
			}
			copy(left, leftLeaf[:])
			copy(right, rightLeaf[:])
			leaves[i] = chainhash.HashH(both)
		}
		leaves = leaves[:len(leaves)>>1]
		leafIndex = halfLeafIndex
	}

	return proof
}

// VerifyInclusionProof returns whether or not the given leaf hash, original
// leaf index, and inclusion proof result in recalculating a merkle root that
// matches the provided merkle root. See GenerateInclusionProof for details
// about the proof.
//
// For example, consider a provided root hash denoted by "h1234o", a leaf hash
// to verify inclusion for denoted by "h2" with a leaf index of 2, and an
// inclusion proof consisting of hashes denoted by "h1o", and "h34o".  The "o"
// here stands for original, as in the original hashes calculated while
// generating the proof.
//
// These values would form the following merkle tree:
//
//	         root = h1234 = h(h12 + h34o)
//	        /                           \
//	  h12 = h(h1o + h2)                h34o
//	   /            \
//	  h1o           h2
//
// The verification will succeed if the root of the new partial merkle tree,
// "h1234", matches the provided root hash "h1234o".
func VerifyInclusionProof(root, leaf *chainhash.Hash, leafIndex uint32, proof []chainhash.Hash) bool {
	// Create a buffer to reuse for hashing the branches and some long lived
	// slices into it to avoid reslicing.
	var buf [2 * chainhash.HashSize]byte
	var left = buf[:chainhash.HashSize]
	var right = buf[chainhash.HashSize:]
	var both = buf[:]

	// Ensure the leaf index to prove is within the possible range per the
	// proof.  First, since the index to prove is a uint32, the maximum possible
	// corresponding proof len is 32.  Second, since the proof is a log2(x)
	// construction, where x is the total number of leaves in the original tree,
	// it follows that the max possible 0-based leaf index is 2^len(proof) - 1.
	proofLen := len(proof)
	if proofLen > 32 {
		return false
	}
	maxLeafIndex := uint32(uint64(1)<<uint8(proofLen) - 1)
	if leafIndex > maxLeafIndex {
		return false
	}

	// The following algorithm works by treating each entry in the proof as the
	// sibling value opposite the known value for each level of the merkle tree
	// and hashing it along with the known value while accounting for whether
	// the known value is in the left or right branch at that level and finally
	// comparing the calculated root to the provided root.
	intermediate := *leaf
	for _, proof := range proof {
		// The sibling hash needed to prove inclusion for the given leaf is on
		// the left when the leaf index for this level of the tree is odd.
		// Otherwise, it's on the right.
		if leafIndex&1 != 0 {
			copy(left, proof[:])
			copy(right, intermediate[:])
		} else {
			copy(left, intermediate[:])
			copy(right, proof[:])
		}
		intermediate = chainhash.HashH(both)

		leafIndex >>= 1
	}

	return *root == intermediate
}
