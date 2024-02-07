insert into num_types(v1, v2, v3, v4, v5, v6) values(182,34453,654385451,453.23,22334.542,120.50);
insert into num_types(v1, v2, v3, v4, v5, v6) values(32762,-3415123,654312385451,999.999,-22334.542,10.4);
insert into num_types(v1, v2, v3, v4, v5, v6) values(-323,53,-90654385451,-459.230,9992334.54290,-12000500.50);

select * from num_types;

insert into decimal_types values(1, 435795334362780682465462748789243337501610978301813276850553121352052192216700289113097427358778598.342434992, 12367890123456789.12);
insert into decimal_types values(2, 790809990636198497784302463464676743730460045716056588284283619572097798777544920701390228264293554.869040822, 55613803484640647.03);
insert into decimal_types values(3, 639331592204741887223305479788137535291488800417414936651322061138931510763125571702251187791371846.884254188, 99999999999999999.99);

insert into datatypes1(bool_type, char_type1, varchar_type, byte_type, enum_type) values(true,'z','this is a string','01010','Mon');
insert into datatypes1(bool_type, char_type1, varchar_type, byte_type, enum_type) values(false,'5','Lorem ipsum dolor sit amet, consectetuer adipiscing elit.','-abcd','Fri');
insert into datatypes1(bool_type, char_type1, varchar_type, byte_type, enum_type) values(true,'z','this is a string','4458','Sun');

select * from datatypes1;

insert into datetime_type(v1, v2, v3, v4) values('1996-12-02', '09:00:00',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP(0));
insert into datetime_type(v1, v2, v3, v4) values('2006-12-02', '12:00:50','2022-11-01 15:55:58.091241',CURRENT_TIMESTAMP(0));
insert into datetime_type(v1, v2, v3, v4) values('1992-01-23', null,current_timestamp,'2022-11-01 15:58:02');

select * from datetime_type;

insert into datetime_type2(v1) values('2022-11-01 15:55:58.091241');
insert into datetime_type2(v1) values('2022-11-01 15:58:02');

select * from datetime_type2;

insert into datatypes2(v1, v2, v3, v4) values ('{"key1": "value1", "key2": "value2"}',B'10'::bit(10),'{20000, 14600, 23500, 13250}', '{{“FD”, “MF”}, {“FD”, “Property”}}');
insert into datatypes2(v1, v2, v3, v4) values ('["a","b","c",1,2,3]',B'100011'::bit(10),'{20000, 14600, 23500, 13250}', '{{“FD”, “MF”}, {"act","two"}}');
insert into datatypes2(v1, v2, v3, v4) values (null,B'1'::bit(10),null, '{{“FD”}, {"act"}}');

select * from datatypes2;

insert into null_and_default (id) VALUES (1);
insert into null_and_default VALUES(2, NULL, NULL, NULL);

