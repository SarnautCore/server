package charstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func validateInventoryExecutionReceipt(receipt InventoryExecutionReceipt) error {
	if receipt.CharacterID == uuid.Nil {
		return fmt.Errorf("%w: inventory execution receipt has no character", ErrConstraintViolated)
	}
	if strings.TrimSpace(receipt.ExecutionKey) != receipt.ExecutionKey || receipt.ExecutionKey == "" {
		return fmt.Errorf("%w: inventory execution key is empty or not canonical", ErrConstraintViolated)
	}
	if strings.TrimSpace(receipt.Operation) != receipt.Operation || receipt.Operation == "" {
		return fmt.Errorf("%w: inventory execution operation is empty or not canonical", ErrConstraintViolated)
	}
	if len(receipt.Result) == 0 {
		return fmt.Errorf("%w: inventory execution receipt has no result", ErrConstraintViolated)
	}
	return nil
}

func (store *memoryStore) LoadInventoryExecutionReceipt(
	ctx context.Context,
	characterID uuid.UUID,
	executionKey string,
) (InventoryExecutionReceipt, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return InventoryExecutionReceipt{}, err
	}
	defer done()
	receipt, ok := state.inventoryReceipts[inventoryReceiptKey{characterID: characterID, executionKey: executionKey}]
	if !ok {
		return InventoryExecutionReceipt{}, ErrNotFound
	}
	receipt.Result = slices.Clone(receipt.Result)
	return receipt, nil
}

func (store *memoryStore) PutInventoryExecutionReceipt(
	ctx context.Context,
	receipt InventoryExecutionReceipt,
) error {
	if err := validateInventoryExecutionReceipt(receipt); err != nil {
		return err
	}
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	key := inventoryReceiptKey{characterID: receipt.CharacterID, executionKey: receipt.ExecutionKey}
	if _, exists := state.inventoryReceipts[key]; exists {
		return ErrExecutionReceiptExists
	}
	receipt.Result = slices.Clone(receipt.Result)
	state.inventoryReceipts[key] = receipt
	return nil
}

func (store *postgresStore) LoadInventoryExecutionReceipt(
	ctx context.Context,
	characterID uuid.UUID,
	executionKey string,
) (InventoryExecutionReceipt, error) {
	const statement = `
		SELECT character_id, execution_key, operation, result
		FROM shard.inventory_execution_receipts
		WHERE character_id = $1 AND execution_key = $2`
	var receipt InventoryExecutionReceipt
	err := store.db.QueryRow(ctx, statement, characterID, executionKey).Scan(
		&receipt.CharacterID, &receipt.ExecutionKey, &receipt.Operation, &receipt.Result,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return InventoryExecutionReceipt{}, ErrNotFound
	}
	if err != nil {
		return InventoryExecutionReceipt{}, fmt.Errorf("load inventory execution receipt: %w", err)
	}
	return receipt, nil
}

func (store *postgresStore) PutInventoryExecutionReceipt(
	ctx context.Context,
	receipt InventoryExecutionReceipt,
) error {
	if err := validateInventoryExecutionReceipt(receipt); err != nil {
		return err
	}
	const statement = `
		INSERT INTO shard.inventory_execution_receipts (
			character_id, execution_key, operation, result
		) VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (character_id, execution_key) DO NOTHING`
	tag, err := store.db.Exec(ctx, statement, receipt.CharacterID, receipt.ExecutionKey, receipt.Operation, receipt.Result)
	if err != nil {
		return classifyConstraint(err, "insert inventory execution receipt")
	}
	if tag.RowsAffected() == 0 {
		return ErrExecutionReceiptExists
	}
	return nil
}
