-- RANGE PARTITIONS
create table ORDER_ITEMS_RANGE_PARTITIONED (
  order_id        integer
                  generated by default on null as identity,
  order_datetime  timestamp not null,
  customer_id     integer not null,
  order_status    varchar2(10 char) not null,
  store_id        integer not null)
PARTITION BY RANGE (order_id,customer_id)
(PARTITION p1 VALUES LESS THAN (50,8),
PARTITION p2 VALUES LESS THAN (70,15),
PARTITION p3 VALUES LESS THAN (90,15))
ENABLE ROW MOVEMENT;
ALTER TABLE ORDER_ITEMS_RANGE_PARTITIONED ADD (CONSTRAINT ord_cus_pk PRIMARY KEY (order_id,customer_id));

-- LIST PARTITIONS
CREATE TABLE ACCOUNTS_LIST_PARTITIONED
( id             NUMBER
, account_number NUMBER
, customer_id    NUMBER
, branch_id      NUMBER
, region         VARCHAR(2)
, status         VARCHAR2(1)
)
PARTITION BY LIST (region)
( PARTITION p_northwest VALUES ('OR', 'WA')
, PARTITION p_southwest VALUES ('AZ', 'UT', 'NM')
, PARTITION p_northeast VALUES ('NY', 'VM', 'NJ')
, PARTITION p_southeast VALUES ('FL', 'GA')
, PARTITION p_northcentral VALUES ('SD', 'WI')
, PARTITION p_southcentral VALUES ('OK', 'TX')
);
ALTER TABLE ACCOUNTS_LIST_PARTITIONED ADD (CONSTRAINT reg_pk PRIMARY KEY (id, region));

--INTERVAL PARTITIONS
CREATE TABLE ORDERS_INTERVAL_PARTITION
  (
    order_id NUMBER 
             GENERATED BY DEFAULT AS IDENTITY START WITH 106 ,
    customer_id NUMBER( 6, 0 ) NOT NULL, -- fk
    status      VARCHAR( 20 ) NOT NULL ,
    salesman_id NUMBER( 6, 0 )         , -- fk
    order_date  DATE NOT NULL   
  )
PARTITION BY RANGE (order_date) 
  INTERVAL(NUMTOYMINTERVAL(1, 'MONTH'))
    ( PARTITION INTERVAL_PARTITION_LESS_THAN_2015 VALUES LESS THAN (TO_DATE('1-1-2015', 'DD-MM-RR')),
      PARTITION INTERVAL_PARTITION_LESS_THAN_2016 VALUES LESS THAN (TO_DATE('1-1-2016', 'DD-MM-RR')),
      PARTITION INTERVAL_PARTITION_LESS_THAN_2017 VALUES LESS THAN (TO_DATE('1-7-2017', 'DD-MM-RR')),
      PARTITION INTERVAL_PARTITION_LESS_THAN_2018 VALUES LESS THAN (TO_DATE('1-1-2024', 'DD-MM-RR')) );
ALTER TABLE ORDERS_INTERVAL_PARTITION ADD (CONSTRAINT ordid_orddate_pk PRIMARY KEY (order_id, order_date));

--HASH PARTITIONS

CREATE TABLE SALES_HASH
  (s_productid  NUMBER,
   s_saledate   DATE,
   s_custid     NUMBER,
   s_totalprice NUMBER)
  PARTITION BY HASH(s_productid)
  ( PARTITION P1
  , PARTITION P2
  , PARTITION P3
  , PARTITION P4
  );

ALTER TABLE SALES_HASH ADD (CONSTRAINT s_prod_id_pk PRIMARY KEY (s_productid, s_custid));


--Multi Level partitions

create table sub_par_test(id integer
                  generated by default on null as identity,emp_name varchar2(30),job_id varchar2(30),hire_date date, PRIMARY KEY(hire_date, job_id))
   partition by range(hire_date) 
   subpartition by list(job_id)(
   Partition P1 Values Less Than(To_Date('01-01-2003','dd-mm-yyyy'))
   (
    Subpartition Sp1 Values('HR_REP','PU_MAN'),
    Subpartition Sp11 Values(Default)
    ),
   Partition P2 Values Less Than(To_Date('01-01-2004','dd-mm-yyyy'))
   (
    subpartition sp2 values('AC_ACCOUNT','FI_ACCOUNT') ,
    Subpartition Sp22 Values(Default)
   ),
    Partition P3 Values Less Than(To_Date('01-01-2005','dd-mm-yyyy'))
    (
    subpartition sp3 values('SH_CLERK','ST_CLERK'),
    subpartition sp33 values(default)
   ),
   Partition P4 Values Less Than(To_Date('01-01-2006','dd-mm-yyyy'))(
    subpartition sp4 values('SA_MAN','PU_MAN'),
    subpartition sp44 values(default)
   ),
   partition p5 values less than(maxvalue)(
    subpartition sp5 values(default)
)) ;

--empty partition tables

CREATE TABLE empty_partition_table
( id             NUMBER
, region         VARCHAR(2)
, status         VARCHAR2(1)
)
PARTITION BY LIST (region)
( PARTITION p_west VALUES ('CA', 'OR')
, PARTITION p_east VALUES ('NY', 'NJ')
);

ALTER TABLE empty_partition_table ADD (CONSTRAINT emp_pk PRIMARY KEY (id, region));

CREATE TABLE empty_partition_table2
( id             NUMBER
, region         VARCHAR(2)
, status         VARCHAR2(1)
, description    VARCHAR2(255)
)
PARTITION BY LIST (region)
( PARTITION p_west VALUES ('CA', 'OR', 'WA')
, PARTITION p_east VALUES ('NY', 'NJ', 'PA')
);

ALTER TABLE empty_partition_table2 ADD (CONSTRAINT emp2_pk PRIMARY KEY (id, region));
