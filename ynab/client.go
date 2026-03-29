package ynab

import (
	"fmt"
	"log"

	"github.com/brunomvsouza/ynab.go"
	"github.com/brunomvsouza/ynab.go/api/category"
	"github.com/brunomvsouza/ynab.go/api/transaction"
)

// Client wraps the YNAB API to provide the operations needed for auto-categorization.
type Client struct {
	client   ynab.ClientServicer
	logger   *log.Logger
	budgetID string
}

// NewClient creates a new YNAB API client.
func NewClient(accessToken, budgetID string, logger *log.Logger) *Client {
	return &Client{
		client:   ynab.NewClient(accessToken),
		logger:   logger,
		budgetID: budgetID,
	}
}

// GetUncategorizedTransactions fetches all uncategorized transactions from the budget.
func (c *Client) GetUncategorizedTransactions() ([]*transaction.Transaction, error) {
	f := &transaction.Filter{
		Type: transaction.StatusUncategorized.Pointer(),
	}

	txns, err := c.client.Transaction().GetTransactions(c.budgetID, f)
	if err != nil {
		return nil, fmt.Errorf("error fetching uncategorized transactions: %w", err)
	}

	c.logger.Printf("fetched %d uncategorized transactions", len(txns))
	return txns, nil
}

// GetCategories fetches all category groups with their categories from the budget.
func (c *Client) GetCategories() ([]*category.GroupWithCategories, error) {
	snapshot, err := c.client.Category().GetCategories(c.budgetID, nil)
	if err != nil {
		return nil, fmt.Errorf("error fetching categories: %w", err)
	}

	c.logger.Printf("fetched %d category groups", len(snapshot.GroupWithCategories))
	return snapshot.GroupWithCategories, nil
}

// UpdateTransactionCategory updates a transaction's category in YNAB.
func (c *Client) UpdateTransactionCategory(txn *transaction.Transaction, categoryID string) error {
	payload := transaction.PayloadTransaction{
		ID:         txn.ID,
		AccountID:  txn.AccountID,
		Date:       txn.Date,
		Amount:     txn.Amount,
		Cleared:    txn.Cleared,
		Approved:   txn.Approved,
		CategoryID: &categoryID,
	}

	_, err := c.client.Transaction().UpdateTransaction(c.budgetID, txn.ID, payload)
	if err != nil {
		return fmt.Errorf("error updating transaction %s: %w", txn.ID, err)
	}

	return nil
}
