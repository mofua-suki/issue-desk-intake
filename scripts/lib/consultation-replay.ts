// Only an authenticated receipt for this exact send ID permits a replay past the local duplicate guard.
// The POST still checks the complete original input on the server.
// readDraft (server/internal/consultations/receipts.go) returns a receipt view when
// the draft is missing or matches the receipt (including an awaiting draft); otherwise
// it returns the ordinary draft. The receipt view has accepted/receiptId/requestId;
// the ordinary draft has id and user messages carrying requestId.
export async function confirmedReceipt(call, id) {
  try {
    const receipt = await call('GET', `/api/drafts/${id}`);
    const confirmed =
      (receipt?.accepted === true &&
        receipt.receiptId === id &&
        receipt.requestId === id) ||
      (receipt?.id === id &&
        Array.isArray(receipt.messages) &&
        receipt.messages.some(
          (message) => message?.role === 'user' && message.requestId === id,
        ));
    return confirmed ? 'confirmed' : 'mismatch';
  } catch (error) {
    return error?.status === 404 ? 'missing' : 'failed';
  }
}
