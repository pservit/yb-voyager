/*
This file contains tests for the trunc function(ORAFCE), reserved words and Mixed Case Tables/Columns.
*/

CREATE TABLE TRUNC_TEST (
  id INTEGER GENERATED BY DEFAULT ON NULL AS IDENTITY, 
  d1 DATE, d2 DATE, d3 DATE, tz1 TIMESTAMP WITH TIME ZONE, 
  tz2 TIMESTAMP WITH TIME ZONE
);
                       
CREATE 
OR REPLACE FUNCTION TRUNC_DATE(d1 in VARCHAR, int_val in VARCHAR) RETURN DATE IS truncated_date DATE;
BEGIN 
SELECT 
  TRUNC(
    TO_DATE(d1, 'DD-MON-YY'), 
    int_val
  ) into truncated_date 
FROM 
  DUAL;
RETURN truncated_date;
END;
/


CREATE 
OR REPLACE FUNCTION TRUNC_TIME_STAMP(d1 in VARCHAR, int_val in VARCHAR) RETURN DATE IS truncated_date_tz TIMESTAMP WITH TIME ZONE;
BEGIN 
SELECT 
  TRUNC(
    TO_TIMESTAMP(d1, 'DD-MON-YY'), 
    int_val
  ) into truncated_date_tz 
FROM 
  DUAL;
RETURN truncated_date_tz;
END;
/


DECLARE d1_date DATE;
d2_date DATE;
d3_date DATE;
tz1_tz TIMESTAMP WITH TIME ZONE;
tz2_ts TIMESTAMP WITH TIME ZONE;
BEGIN d1_date := TRUNC_DATE('02-MAR-15', 'mi');
d2_date := TRUNC_DATE('02-MAR-16', 'ww');
d3_date := TRUNC_TIME_STAMP('02-MAR-18', 'mm');
tz1_tz := TRUNC_TIME_STAMP('02-MAR-18', 'mm');
tz2_ts := TRUNC_TIME_STAMP('02-MAR-19', 'YEAR');
INSERT INTO TRUNC_TEST (d1, d2, d3, tz1, tz2) 
VALUES 
  (
    d1_date, d2_date, d3_date, tz1_tz, 
    tz2_ts
  );
END;
/


CREATE TABLE test_timezone(id integer primary key, dtts TIMESTAMP(9));
ALTER TABLE test_timezone add constraint test_cc1 check((dtts = trunc(dtts)));

INSERT INTO test_timezone values (1,'2-NOV-92');


CREATE TABLE "group" (
    id int PRIMARY KEY,
    name varchar(10)
);

CREATE TABLE reserved_column (
    "user" int,
    "case" varchar(10)
);

CREATE TABLE "check" (
    "user" int,
    "case" varchar(10)
);

INSERT into "group" values(1, 'abc');
INSERT into "group" values(2, 'abc');
INSERT into "group" values(3, 'abc');
INSERT into "group" values(4, 'abc');
INSERT into "group" values(5, 'abc');

INSERT into reserved_column values(1, 'abc');
INSERT into reserved_column values(2, 'abc');
INSERT into reserved_column values(3, 'abc');
INSERT into reserved_column values(4, 'abc');
INSERT into reserved_column values(5, 'abc');

INSERT into "check" values(1, 'abc');
INSERT into "check" values(2, 'abc');
INSERT into "check" values(3, 'abc');
INSERT into "check" values(4, 'abc');
INSERT into "check" values(5, 'abc');

CREATE TABLE "Mixed_Case_Table_Name_Test" (
	id int GENERATED BY DEFAULT AS IDENTITY,
	first_name VARCHAR(50) not null,
	last_name VARCHAR(50),
	email VARCHAR(50),
	gender VARCHAR(50),
	ip_address VARCHAR(20)
);
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Modestine', 'MacMeeking', 'mmacmeeking0@zimbio.com', 'Female', '208.44.58.185');
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Genna', 'Kaysor', 'gkaysor1@hibu.com', 'Female', '202.48.51.58');
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Tess', 'Wesker', 'twesker2@scientificamerican.com', 'Female', '177.153.32.186');
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Magnum', 'Danzelman', 'mdanzelman3@storify.com', 'Bigender', '192.200.33.56');
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Mitzi', 'Pidwell', 'mpidwell4@shutterfly.com', 'Female', '216.4.250.71');
insert into "Mixed_Case_Table_Name_Test" (first_name, last_name, email, gender, ip_address) values ('Milzie', 'Rohlfing', 'mrohlfing5@java.com', 'Female', '230.101.87.42');

CREATE TABLE "Case_Sensitive_Columns" (
	id int GENERATED BY DEFAULT AS IDENTITY,
	"First_Name" VARCHAR(50) not null,
	last_name VARCHAR(50),
	email VARCHAR(50),
	gender VARCHAR(50),
	ip_address VARCHAR(20)
);
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Modestine', 'MacMeeking', 'mmacmeeking0@zimbio.com', 'Female', '208.44.58.185');
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Genna', 'Kaysor', 'gkaysor1@hibu.com', 'Female', '202.48.51.58');
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Tess', 'Wesker', 'twesker2@scientificamerican.com', 'Female', '177.153.32.186');
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Magnum', 'Danzelman', 'mdanzelman3@storify.com', 'Bigender', '192.200.33.56');
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Mitzi', 'Pidwell', 'mpidwell4@shutterfly.com', 'Female', '216.4.250.71');
insert into "Case_Sensitive_Columns" ("First_Name", last_name, email, gender, ip_address) values ('Milzie', 'Rohlfing', 'mrohlfing5@java.com', 'Female', '230.101.87.42');
