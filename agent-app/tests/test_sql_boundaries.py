import pytest
from app.tools.scan import scan_sql

@pytest.mark.parametrize('sql', [
    'DELETE FROM orders RETURNING *;', 'DELETE FROM "orders";',
    "UPDATE orders SET note = 'where';", 'DELETE FROM orders /* where id=1 */;',
    'UPDATE orders SET note = $$where$$;',
    'UPDATE orders SET note = (SELECT note FROM other WHERE id=1);',
    'WITH gone AS (DELETE FROM orders RETURNING *) SELECT * FROM gone WHERE id=1;',
    'DELETE FROM orders; SELECT * FROM orders WHERE id=1;',
])
def test_unconditional_dml_is_blocked(sql):
    assert scan_sql(sql, 'SELECT 1;').status == 'BLOCKED'

@pytest.mark.parametrize('sql', [
    'DO $$ BEGIN UPDATE orders SET x=1; END $$;',
    'DO $$ BEGIN DELETE FROM orders; END $$;',
    'DO $body$ BEGIN UPDATE orders SET x=1; END $body$;',
    'CREATE FUNCTION f() RETURNS void AS $$ BEGIN DELETE FROM orders; END $$ LANGUAGE plpgsql;',
    'CREATE OR REPLACE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$ BEGIN DELETE FROM orders; END $$;',
    'CREATE PROCEDURE p() LANGUAGE sql AS $$ DELETE FROM orders; $$;',
    'CREATE TEMP FUNCTION f() RETURNS void AS $$ BEGIN DELETE FROM orders; END $$ LANGUAGE plpgsql;',
    'CREATE OR REPLACE TEMPORARY FUNCTION f() RETURNS void AS $$ BEGIN DELETE FROM orders; END $$ LANGUAGE plpgsql;',
    'BEGIN ATOMIC DELETE FROM orders; END;',
])
def test_opaque_procedural_body_requires_review(sql):
    check = scan_sql(sql, 'SELECT 1;')
    assert check.status == 'BLOCKED'
    assert 'PROCEDURAL_BODY_REQUIRES_REVIEW' in [item.code for item in check.items]

@pytest.mark.parametrize('sql', [
    'SELECT $$DELETE FROM orders$$;', "SELECT 'DELETE FROM orders';",
    'SELECT $body$UPDATE orders SET x=1$body$;',
])
def test_dollar_quoted_literals_are_not_procedural_bodies(sql):
    assert scan_sql(sql, 'SELECT 1;').status == 'PASSED'

@pytest.mark.parametrize('sql', [
    'MERGE INTO orders o USING s ON o.id=s.id WHEN MATCHED THEN UPDATE SET x=1;',
    'MERGE INTO orders o USING s ON o.id=s.id WHEN MATCHED THEN UPDATE SET x=1 WHEN NOT MATCHED THEN INSERT (id) VALUES (s.id);',
    'MERGE INTO orders AS o USING (SELECT * FROM staging WHERE id=1) AS s ON o.id=s.id '
    'WHEN MATCHED AND s.amount < 0 THEN UPDATE SET amount=0;',
])
def test_merge_update_is_not_unconditional(sql):
    assert scan_sql(sql, 'SELECT 1;').status == 'PASSED'

def test_merge_without_on_still_reports_unconditional_update():
    # Guard against over-correcting: only MERGE-scoped UPDATEs are exempt.
    check = scan_sql('UPDATE orders SET x=1;', 'SELECT 1;')
    assert 'UPDATE_WITHOUT_WHERE' in [item.code for item in check.items]

@pytest.mark.parametrize('sql', [
    'CREATE TABLE t (function text);',
    'CREATE TABLE t (procedure text);',
    'CREATE TABLE t (function_name text, procedure_id int);',
    'CREATE INDEX CONCURRENTLY i ON t (function);',
    'ALTER TABLE t ADD COLUMN function text;',
    'SELECT function FROM t WHERE id=1;',
])
def test_create_table_with_function_like_columns_is_not_a_procedural_body(sql):
    """对象类型只看 CREATE 之后的位置，不看整条语句里是否出现过这个词。"""
    check = scan_sql(sql, 'SELECT 1;')
    assert 'PROCEDURAL_BODY_REQUIRES_REVIEW' not in [item.code for item in check.items]

@pytest.mark.parametrize('sql', [
    'DELETE FROM "orders" WHERE id=1 RETURNING *;',
    "UPDATE orders SET note='where' WHERE id=1;",
    'SELECT * FROM orders FOR UPDATE;',
    'SELECT * FROM orders FOR NO KEY UPDATE;',
    'INSERT INTO orders(id) VALUES (1) ON CONFLICT(id) DO UPDATE SET id=1;',
    "SELECT 'DELETE FROM orders';", 'SELECT $$DELETE FROM orders$$;',
    '/* outer /* DELETE FROM orders */ comment */ SELECT 1;',
])
def test_literals_comments_and_non_dml_not_misclassified(sql):
    assert scan_sql(sql, 'SELECT 1;').status == 'PASSED'

@pytest.mark.parametrize('sql', ["SELECT 'unterminated", 'DELETE FROM "orders', '/* missing', 'SELECT $tag$missing', 'SELECT (1;'])
def test_ambiguous_lexical_input_fails_closed(sql):
    assert scan_sql(sql, 'SELECT 1;').status == 'FAILED'
