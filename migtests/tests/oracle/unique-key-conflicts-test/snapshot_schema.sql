-- TODO
-- 2 different unique constraints on the same table
-- 2 different unique indexes on the same table

-- Table with Single Column Unique Constraint
CREATE TABLE single_unique_constr (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    email VARCHAR2(255) UNIQUE
);

-- Table with Multiple Column Unique Constraint
CREATE TABLE multi_unique_constr (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    first_name VARCHAR2(100),
    last_name VARCHAR2(100),
    CONSTRAINT unique_name UNIQUE (first_name, last_name)
);

-- Table with Single Column Unique Index
CREATE TABLE single_unique_idx (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    ssn VARCHAR2(100)
);
CREATE UNIQUE INDEX idx_ssn_unique ON single_unique_idx (ssn);

-- Table with Multiple Column Unique Index
CREATE TABLE multi_unique_idx (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    first_name VARCHAR2(100),
    last_name VARCHAR2(100)
);
CREATE UNIQUE INDEX idx_name_unique ON multi_unique_idx (first_name, last_name);

-- Table with Unique Constraint and Unique Index on Different Columns
CREATE TABLE diff_columns_constr_idx (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    email VARCHAR2(255) UNIQUE,
    phone_number VARCHAR2(20)
);
CREATE UNIQUE INDEX idx_phone_unique ON diff_columns_constr_idx (phone_number);

-- Table with Unique Constraint and Unique Index, with Overlapping Columns
CREATE TABLE subset_columns_constr_idx (
    id NUMBER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    first_name VARCHAR2(100),
    last_name VARCHAR2(100),
    phone_number VARCHAR2(20)
);

-- Unique constraint on first_name and last_name
ALTER TABLE subset_columns_constr_idx ADD CONSTRAINT unique_name_constraint UNIQUE (first_name, last_name);

-- Unique index on first_name, last_name, and phone_number
CREATE UNIQUE INDEX idx_name_phone_unique ON subset_columns_constr_idx (first_name, last_name, phone_number);