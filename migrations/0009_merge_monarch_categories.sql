-- Fold Monarch's finer categories (from the 2026-09-30 import) into broader ones, matching monarchCategories.
UPDATE transactions SET user_category = CASE user_category
    WHEN 'Financial Fees' THEN 'Fees' WHEN 'Financial & Legal Services' THEN 'Fees'
    WHEN 'Office Supplies & Expenses' THEN 'Work Expenses' WHEN 'Business Utilities & Communication' THEN 'Work Expenses'
    WHEN 'Business Insurance' THEN 'Work Expenses' WHEN 'Business Auto Expenses' THEN 'Work Expenses'
    WHEN 'Postage & Shipping' THEN 'Shopping' WHEN 'Cash & ATM' THEN 'Cash & Checks' WHEN 'Check' THEN 'Cash & Checks'
    WHEN 'Fun Money' THEN 'Other' ELSE user_category END
  WHERE user_category IN ('Financial Fees','Financial & Legal Services','Office Supplies & Expenses','Business Utilities & Communication',
    'Business Insurance','Business Auto Expenses','Postage & Shipping','Cash & ATM','Check','Fun Money');
