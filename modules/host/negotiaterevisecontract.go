package host

// TODO: Since we're gathering untrusted input, need to check for both
// overflows and nil values.

// TODO: Does the host properly account for the cost of uploading or
// downloading data? Sectors gained is not going to be good enough, because
// it's going to contain a whole sector even though the amount of storage is
// not changing and the amount of bandwidth is mostly minimal.

import (
	"errors"
	"net"
	"time"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

var (
	// errBadModificationIndex is returned if the renter requests a change on a
	// sector root that is not in the file contract.
	errBadModificationIndex = errors.New("renter has made a modification that points to a nonexistant sector")

	// badSectorSize is returned if the renter provides a sector to be inserted
	// that is the wrong size.
	errBadSectorSize = errors.New("renter has provided an incorrectly sized sector")

	// errIllegalOffsetAndLength is returned if the renter tries perform a
	// modify operation that uses a troublesome combination of offset and
	// length.
	errIllegalOffsetAndLength = errors.New("renter is trying to do a modify with an illegal offset and length")

	// errLargeSector is returned if the renter sends a RevisionAction that has
	// data which creates a sector that is larger than what the host uses.
	errLargeSector = errors.New("renter has sent a sector that exceeds the host's sector size")

	// errUnknownModification is returned if the host receives a modification
	// action from the renter that it does not understand.
	errUnknownModification = errors.New("renter is attempting an action that the host is not aware of")
)

// managedRevisionIteration handles one iteration of the revision loop. As a
// performance optimization, multiple iterations of revisions are allowed to be
// made over the same connection.
func (h *Host) managedRevisionIteration(conn net.Conn, so *storageObligation) error {
	// Send the settings to the renter. The host will keep going even if it is
	// not accepting contracts, because in this case the contract already
	// exists.
	err := h.managedRPCSettings(conn)
	if err != nil {
		return err
	}

	// Set the negotiation deadline.
	conn.SetDeadline(time.Now().Add(modules.NegotiateFileContractRevisionTime))

	// The renter will either accept or reject the settings + revision
	// transaction. It may also return a stop response to indicate that it
	// wishes to terminate the revision loop.
	err = modules.ReadNegotiationAcceptance(conn)
	if err != nil {
		return err
	}

	// Read some variables from the host for use later in the function.
	h.mu.RLock()
	settings := h.settings
	secretKey := h.secretKey
	blockHeight := h.blockHeight
	h.mu.RUnlock()

	// The renter is now going to send a batch of modifications followed by an
	// updated file contract revision. Read the number of modifications being
	// sent by the renter.
	var modifications []modules.RevisionAction
	err = encoding.ReadObject(conn, &modifications, settings.MaxReviseBatchSize)
	if err != nil {
		return err
	}

	// First read all of the modifications. Then make the modifications, but
	// with the ability to reverse them. Then verify the the file contract
	// revision that comes down the line.
	var bandwidthRevenue types.Currency
	var storageRevenue types.Currency
	var collateralRisked types.Currency
	var sectorsRemoved []crypto.Hash
	var sectorsGained []crypto.Hash
	var gainedSectorData [][]byte
	err = func() error {
		for _, modification := range modifications {
			// Check that the index points to an existing sector root. If the type
			// is ActionInsert, we permit inserting at the end.
			if modification.Type == modules.ActionInsert {
				if modification.SectorIndex > uint64(len(so.SectorRoots)) {
					return errBadModificationIndex
				}
			} else if modification.SectorIndex >= uint64(len(so.SectorRoots)) {
				return errBadModificationIndex
			}
			// Check that the data sent for the sector is not too large.
			if uint64(len(modification.Data)) > modules.SectorSize {
				return errLargeSector
			}

			// Run a different codepath depending on the renter's selection.
			if modification.Type == modules.ActionDelete {
				// There is no financial information to change, it is enough to
				// remove the sector.
				sectorsRemoved = append(sectorsRemoved, so.SectorRoots[modification.SectorIndex])
				so.SectorRoots = append(so.SectorRoots[0:modification.SectorIndex], so.SectorRoots[modification.SectorIndex+1:]...)
			} else if modification.Type == modules.ActionInsert {
				// Check that the sector size is correct.
				if uint64(len(modification.Data)) != modules.SectorSize {
					return errBadSectorSize
				}

				// Update finances.
				blocksRemaining := so.proofDeadline() - blockHeight
				blockBytesCurrency := types.NewCurrency64(uint64(blocksRemaining)).Mul(types.NewCurrency64(modules.SectorSize))
				bandwidthRevenue = bandwidthRevenue.Add(settings.MinimumUploadBandwidthPrice.Mul(types.NewCurrency64(modules.SectorSize)))
				storageRevenue = storageRevenue.Add(settings.MinimumStoragePrice.Mul(blockBytesCurrency))
				collateralRisked = collateralRisked.Add(settings.Collateral.Mul(blockBytesCurrency))

				// Insert the sector into the root list.
				newRoot := crypto.MerkleRoot(modification.Data)
				sectorsGained = append(sectorsGained, newRoot)
				gainedSectorData = append(gainedSectorData, modification.Data)
				so.SectorRoots = append(so.SectorRoots[:modification.SectorIndex], append([]crypto.Hash{newRoot}, so.SectorRoots[modification.SectorIndex:]...)...)
			} else if modification.Type == modules.ActionModify {
				// Check that the offset and length are okay. Length is already
				// known to be appropriately small, but the offset needs to be
				// checked for being appropriately small as well otherwise there is
				// a risk of overflow.
				if modification.Offset > modules.SectorSize || modification.Offset+uint64(len(modification.Data)) > modules.SectorSize {
					return errIllegalOffsetAndLength
				}

				// Get the data for the new sector.
				sector, err := h.readSector(so.SectorRoots[modification.SectorIndex])
				if err != nil {
					return err
				}
				copy(sector[modification.Offset:], modification.Data)

				// Update finances.
				bandwidthRevenue = bandwidthRevenue.Add(settings.MinimumUploadBandwidthPrice.Mul(types.NewCurrency64(modules.SectorSize)))

				// Update the sectors removed and gained to indicate that the old
				// sector has been replaced with a new sector.
				newRoot := crypto.MerkleRoot(sector)
				sectorsRemoved = append(sectorsRemoved, so.SectorRoots[modification.SectorIndex])
				sectorsGained = append(sectorsGained, newRoot)
				gainedSectorData = append(gainedSectorData, sector)
				so.SectorRoots[modification.SectorIndex] = newRoot
			} else {
				return errUnknownModification
			}
		}
		return nil
	}()
	if err != nil {
		return modules.WriteNegotiationRejection(conn, err)
	}

	// Read the file contract revision and check whether it's acceptable.
	var revision types.FileContractRevision
	err = encoding.ReadObject(conn, &revision, 16e3)
	if err != nil {
		return err
	}
	err = verifyRevision(so, revision, storageRevenue, bandwidthRevenue, collateralRisked)
	if err != nil {
		return modules.WriteNegotiationRejection(conn, err)
	}

	// Revision is acceptable, write an acceptance string.
	err = modules.WriteNegotiationAcceptance(conn)
	if err != nil {
		return err
	}

	// Renter will now send the transaction signatures for the file contract
	// revision.
	var renterSig types.TransactionSignature
	err = encoding.ReadObject(conn, &renterSig, 16e3)
	if err != nil {
		return err
	}

	// Create the signatures for a transaction that contains only the file
	// contract revision and the renter signatures.
	// Create the CoveredFields for the signature.
	cf := types.CoveredFields{
		FileContractRevisions: []uint64{0},
	}
	hostTxnSig := types.TransactionSignature{
		ParentID:       crypto.Hash(revision.ParentID),
		PublicKeyIndex: 1,
		CoveredFields:  cf,
	}
	txn := types.Transaction{
		FileContractRevisions: []types.FileContractRevision{revision},
		TransactionSignatures: []types.TransactionSignature{renterSig, hostTxnSig},
	}
	sigHash := txn.SigHash(1)
	encodedSig, err := crypto.SignHash(sigHash, secretKey)
	if err != nil {
		return err
	}
	txn.TransactionSignatures[1].Signature = encodedSig[:]

	// Host will verify the transaction StandaloneValid is enough. If valid,
	// the host will update and submit the storage obligation.
	err = txn.StandaloneValid(blockHeight)
	if err != nil {
		return modules.WriteNegotiationRejection(conn, err)
	}
	// Verify that the renter signature is covering the right fields.
	if renterSig.CoveredFields.WholeTransaction {
		return errors.New("renter cannot cover the whole transaction")
	}
	so.AnticipatedRevenue = so.AnticipatedRevenue.Add(storageRevenue)
	so.ConfirmedRevenue = so.ConfirmedRevenue.Add(bandwidthRevenue)
	so.RiskedCollateral = so.RiskedCollateral.Add(collateralRisked)
	err = h.modifyStorageObligation(so, sectorsRemoved, sectorsGained, gainedSectorData)
	if err != nil {
		return modules.WriteNegotiationRejection(conn, err)
	}

	// Host will now send acceptance and its signature to the renter. This
	// iteration is complete.
	err = modules.WriteNegotiationAcceptance(conn)
	if err != nil {
		return err
	}
	return encoding.WriteObject(conn, txn.TransactionSignatures[1])
}

// managedRPCReviseContract accepts a request to revise an existing contract.
// Revisions can add sectors, delete sectors, and modify existing sectors.
func (h *Host) managedRPCReviseContract(conn net.Conn) error {
	// Set a preliminary deadline for receiving the storage obligation.
	startTime := time.Now()
	// Perform the file contract revision exchange, giving the renter the most
	// recent file contract revision and getting the storage obligation that
	// will be used to pay for the data.
	_, so, err := h.managedRPCRecentRevision(conn)
	if err != nil {
		return err
	}

	// Lock the storage obligation during the revision.
	err = h.lockStorageObligation(so)
	if err != nil {
		return err
	}
	defer func() {
		err = h.unlockStorageObligation(so)
		if err != nil {
			h.log.Critical(err)
		}
	}()

	// Begin the revision loop. The host will process revisions until a
	// timeout is reached, or until the renter sends a StopResponse.
	for time.Now().Before(startTime.Add(1200 * time.Second)) {
		err := h.managedRevisionIteration(conn, so)
		if err == modules.ErrStopResponse {
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}

// verifyRevision checks that the revision
//
// TODO: Finish implementation
func verifyRevision(so *storageObligation, revision types.FileContractRevision, storageRevenue, bandwidthRevenue, collateralRisked types.Currency) error {
	// Check that all non-volatile fields are the same.

	// Check that the root hash and the file size match the updated sector
	// roots.

	// Check that the payments have updated to reflect the new revenues.

	// Check that the revision number has increased.

	// Check any other thing that needs to be checked.
	return nil
}
